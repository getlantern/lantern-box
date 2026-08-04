package ruleset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/srs"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

// Constants representing different types of filters currently supported.
const (
	TypeDomain        = "domain"
	TypeDomainSuffix  = "domainSuffix"
	TypeDomainKeyword = "domainKeyword"
	TypeDomainRegex   = "domainRegex"
	TypeProcessName   = "processName"
	TypePackageName   = "packageName"
)

// MutableRuleSet allows adding/removing items to/from an [adapter.RuleSet] filter and the ability
// enable/disable all rules currently using the ruleset.
type MutableRuleSet struct {
	tag     string
	enabled *atomic.Bool

	router   adapter.Router
	ruleset  adapter.RuleSet
	rules    []adapter.Rule
	filter   option.DefaultHeadlessRule
	filterMu sync.RWMutex
	started  bool
	startMu  sync.Mutex

	ruleFile string
	// fileFormat is the format of the rule file.
	fileFormat string
}

// newMutableRuleSet returns a new [MutableRuleSet] with the given tag, dataPath, and enable state.
// format should be either [constant.RuleSetFormatSource] or [constant.RuleSetFormatBinary].
func newMutableRuleSet(dataPath, tag, format string, enable bool) (*MutableRuleSet, error) {
	// TODO: accept logger? Probably should..
	var path string
	switch format {
	case constant.RuleSetFormatSource, "":
		path = filepath.Join(dataPath, tag+".json")
	case constant.RuleSetFormatBinary:
		path = filepath.Join(dataPath, tag+".srs")
	default:
		return nil, fmt.Errorf("unsupported rule file format: %s", format)
	}

	enabled := new(atomic.Bool)
	enabled.Store(enable)
	m := &MutableRuleSet{
		tag:        tag,
		enabled:    enabled,
		filter:     option.DefaultHeadlessRule{},
		ruleFile:   path,
		fileFormat: format,
	}

	// check if ruleFile exists and create it if it doesn't
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("accessing rule file %s: %w", path, err)
		}
		if err := m.saveToFile(); err != nil {
			return nil, fmt.Errorf("saving rule file %s: %w", path, err)
		}
	}
	return m, nil
}

// Enable enables all rules using the managed ruleset
func (m *MutableRuleSet) Enable() {
	m.enabled.Store(true)
}

// Disable disables all rules using the managed ruleset
func (m *MutableRuleSet) Disable() {
	m.enabled.Store(false)
}

func (m *MutableRuleSet) IsEnabled() bool {
	return m.enabled.Load()
}

// Start initializes the [MutableRuleSet] with the given context.
// Note, because [MutableRuleSet] uses the [adapter.Router] to find rules associated with the ruleset,
// the [adapter.Router] must be created and added to ctx before calling Start.
func (m *MutableRuleSet) Start(ctx context.Context) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	if m.started {
		return nil
	}

	router := service.FromContext[adapter.Router](ctx)
	if router == nil {
		return fmt.Errorf("router not found in context")
	}
	ruleset, loaded := router.RuleSet(m.tag)
	if !loaded {
		return fmt.Errorf("%v ruleSet not found", m.tag)
	}
	if _, ok := ruleset.(*rule.LocalRuleSet); !ok {
		return fmt.Errorf("unsupported ruleset type: %T", ruleset)
	}
	m.ruleset = ruleset

	// find all rules associated with the ruleset, wrap them, and then insert ourselves back into
	// the router. we also keep a reference to the rules to allow for enabling/disabling them.
	rules := router.Rules()
	for i, r := range rules {
		if strings.Contains(r.String(), "rule_set="+m.tag) {
			s := &ruleWrapper{
				Rule:    r,
				name:    m.tag,
				enabled: m.enabled,
			}
			rules[i] = s
			m.rules = append(m.rules, s)
		}
	}

	// load filters from the ruleset and register a callback to reload filters when the ruleset
	// changes so we're always in sync.
	ruleset.RegisterCallback(m.loadFilters)
	m.loadFilters(ruleset)
	m.started = true
	return nil
}

// Filters returns the current filters
func (m *MutableRuleSet) Filters() option.DefaultHeadlessRule {
	m.filterMu.RLock()
	defer m.filterMu.RUnlock()
	return option.DefaultHeadlessRule{
		Domain:        slices.Clone(m.filter.Domain),
		DomainSuffix:  slices.Clone(m.filter.DomainSuffix),
		DomainKeyword: slices.Clone(m.filter.DomainKeyword),
		DomainRegex:   slices.Clone(m.filter.DomainRegex),
		ProcessName:   slices.Clone(m.filter.ProcessName),
		PackageName:   slices.Clone(m.filter.PackageName),
	}
}

// AddItem adds a new item to the filter of the given type. It returns an error if the filter type is
// unsupported or it fails to update the rule file.
func (m *MutableRuleSet) AddItem(filterType, item string) error {
	if err := m.modify(filterType, item, merge); err != nil {
		return err
	}
	if err := m.saveToFile(); err != nil {
		m.modify(filterType, item, remove)
		return fmt.Errorf("writing rule to %s: %w", m.ruleFile, err)
	}
	return nil
}

// RemoveItem removes an item from the filter of the given type. It retruns an error if the filter
// type is unsupported or it fails to update the rule file.
func (m *MutableRuleSet) RemoveItem(filterType, item string) error {
	if err := m.modify(filterType, item, remove); err != nil {
		return err
	}
	if err := m.saveToFile(); err != nil {
		return fmt.Errorf("writing rule to %s: %w", m.ruleFile, err)
	}
	return nil
}

// AddItems merges the given [option.DefaultHeadlessRule] with the current filters, ignoring unsupported
// fields. It returns an error if it fails to update the rule file.
func (m *MutableRuleSet) AddItems(items option.DefaultHeadlessRule) error {
	m.patch(items, merge)
	return m.saveToFile()
}

// RemoveItems removes multiple items from the filter. It retruns an error if the filter type is
// unsupported or it fails to update the rule file.
func (m *MutableRuleSet) RemoveItems(items option.DefaultHeadlessRule) error {
	m.patch(items, remove)
	return m.saveToFile()
}

type actionFn func(slice []string, items []string) []string

// modify uses fn to modify the filter of the given type with the given item.
func (m *MutableRuleSet) modify(filterType string, item string, fn actionFn) error {
	m.filterMu.Lock()
	defer m.filterMu.Unlock()

	items := []string{item}
	switch filterType {
	case TypeDomain:
		m.filter.Domain = fn(m.filter.Domain, items)
	case TypeDomainSuffix:
		m.filter.DomainSuffix = fn(m.filter.DomainSuffix, items)
	case TypeDomainKeyword:
		m.filter.DomainKeyword = fn(m.filter.DomainKeyword, items)
	case TypeDomainRegex:
		m.filter.DomainRegex = fn(m.filter.DomainRegex, items)
	case TypeProcessName:
		m.filter.ProcessName = fn(m.filter.ProcessName, items)
	case TypePackageName:
		m.filter.PackageName = fn(m.filter.PackageName, items)
	default:
		return fmt.Errorf("unsupported filter type: %s", filterType)
	}
	return nil
}

// patch uses fn to modify the filter with the given diff.
func (m *MutableRuleSet) patch(diff option.DefaultHeadlessRule, fn actionFn) {
	m.filterMu.Lock()
	defer m.filterMu.Unlock()
	if len(diff.Domain) > 0 {
		m.filter.Domain = fn(m.filter.Domain, diff.Domain)
	}
	if len(diff.DomainSuffix) > 0 {
		m.filter.DomainSuffix = fn(m.filter.DomainSuffix, diff.DomainSuffix)
	}
	if len(diff.DomainKeyword) > 0 {
		m.filter.DomainKeyword = fn(m.filter.DomainKeyword, diff.DomainKeyword)
	}
	if len(diff.DomainRegex) > 0 {
		m.filter.DomainRegex = fn(m.filter.DomainRegex, diff.DomainRegex)
	}
	if len(diff.ProcessName) > 0 {
		m.filter.ProcessName = fn(m.filter.ProcessName, diff.ProcessName)
	}
	if len(diff.PackageName) > 0 {
		m.filter.PackageName = fn(m.filter.PackageName, diff.PackageName)
	}
}

func merge(slice []string, items []string) []string {
	return append(slice, items...)
}

func remove(slice []string, items []string) []string {
	if len(slice) == 0 || len(items) == 0 {
		return slice
	}

	if len(items) == 1 {
		idx := slices.Index(slice, items[0])
		if idx == -1 {
			return slice
		}
		j := len(slice) - 1
		slice[idx] = slice[j]
		return slice[:j]
	}

	itemSet := make(map[string]struct{}, len(items))
	for _, item := range items {
		itemSet[item] = struct{}{}
	}

	i, j := 0, len(slice)-1
	for i <= j {
		v := slice[i]
		if _, found := itemSet[v]; found {
			slice[i] = slice[j]
			j--
		} else {
			i++
		}
	}
	return slice[:i]
}

// saveToFile saves the current filter to the rule file.
func (m *MutableRuleSet) saveToFile() error {
	m.filterMu.RLock()
	filters := m.filter
	m.filterMu.RUnlock()
	rs := option.PlainRuleSetCompat{
		Version: 3,
		Options: option.PlainRuleSet{
			Rules: []option.HeadlessRule{
				{
					Type:           "default",
					DefaultOptions: filters,
				},
			},
		},
	}
	switch m.fileFormat {
	case constant.RuleSetFormatSource, "":
		buf, err := json.Marshal(rs)
		if err != nil {
			return err
		}
		return os.WriteFile(m.ruleFile, buf, 0644)
	case constant.RuleSetFormatBinary:
		setFile, err := os.Open(m.ruleFile)
		if err != nil {
			return err
		}
		if err := srs.Write(setFile, rs.Options, rs.Version); err != nil {
			setFile.Close()
			return err
		}
		setFile.Close()
		return nil
	}
	return fmt.Errorf("unsupported rule file format: %s", m.fileFormat)
}

// loadFilters loads filters from the given [adapter.RuleSet].
func (m *MutableRuleSet) loadFilters(s adapter.RuleSet) {
	// we can safely ignore all errors in this function since the wrapped rule.LocalRuleSet would have
	// already failed before calling this function. if we really want to do sanity checks, we can
	// log them.
	var ruleSet option.PlainRuleSetCompat
	switch m.fileFormat {
	case constant.RuleSetFormatSource, "":
		content, _ := os.ReadFile(m.ruleFile)
		ruleSet, _ = json.UnmarshalExtended[option.PlainRuleSetCompat](content)
	case constant.RuleSetFormatBinary:
		setFile, _ := os.Open(m.ruleFile)
		ruleSet, _ = srs.Read(setFile, false)
		setFile.Close()
	default:
		return
	}

	m.filterMu.Lock()
	rules := ruleSet.Options.Rules
	if len(rules) == 0 {
		m.filter = option.DefaultHeadlessRule{}
	} else {
		m.filter = ruleSet.Options.Rules[0].DefaultOptions
	}
	m.filterMu.Unlock()
}

// RuleFilePath returns the path to the rule file.
func (m *MutableRuleSet) RuleFilePath() string {
	return m.ruleFile
}

// ruleWrapper wraps an [adapter.Rule] to allow checking if the rule is enabled before matching.
type ruleWrapper struct {
	// Rule is the underlying rule.
	adapter.Rule
	name    string
	enabled *atomic.Bool
}

// Match checks if the ruleset is enabled and the wrapped [adapter.Rule] matches metadata.
func (s *ruleWrapper) Match(metadata *adapter.InboundContext) bool {
	return s.enabled.Load() && s.Rule.Match(metadata)
}

// Type returns the rule type.
func (s *ruleWrapper) Type() string {
	return s.Rule.Type() + "-" + s.name
}

func (s *ruleWrapper) String() string {
	return fmt.Sprintf("%s enabled=%v", s.Rule.String(), s.enabled.Load())
}
