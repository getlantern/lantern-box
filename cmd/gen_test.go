package main

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

func TestGenOpts(t *testing.T) {
	testGenOpts(t, config{
		Type:       constant.TypeHTTP,
		Tag:        "http-in",
		ListenPort: 8080,
		TLS: &tlsConfig{
			Insecure:   true,
			ServerName: "example.com",
			CertPath:   "/path/to/cert.pem",
			KeyPath:    "/path/to/key.pem",
		},
	})
}

func TestSetUserOptions(t *testing.T) {
	testGenOpts(t, config{
		Type:       constant.TypeVMess,
		Tag:        "http-in",
		ListenPort: 8080,
		TLS: &tlsConfig{
			Insecure:   true,
			ServerName: "example.com",
		},
		User: map[string]any{
			"Name":    "test-user",
			"UUID":    "123e4567-e89b-12d3-a456-426614174000",
			"AlterId": 32,
		},
	})
}

func testGenOpts(t *testing.T, cfg config) {
	options, err := genOpts(cfg)
	require.NoError(t, err)

	ctx := box.BaseContext()
	buf, err := json.MarshalContext(ctx, options)
	require.NoError(t, err)
	fmt.Println(string(buf))

	require.NoError(t, checkValid(ctx, options))
}

func TestFillExample(t *testing.T) {
	opts := option.HTTPOutboundOptions{}
	FillExample(&opts)
	opts.DialerOptions = option.DialerOptions{}

	buf, err := json.MarshalContext(box.BaseContext(), opts)
	require.NoError(t, err)

	var b bytes.Buffer
	stdjson.Indent(&b, buf, "", "  ")
	fmt.Println(b.String())
}

func FillExample(v any) {
	fillExample(reflect.ValueOf(v))
}

func fillExample(val reflect.Value) {
	if val.Kind() == reflect.Ptr {
		if val.IsNil() {
			val.Set(reflect.New(val.Type().Elem()))
		}
		fillExample(val.Elem())
		return
	}
	if val.Kind() == reflect.Struct {
		for i := 0; i < val.NumField(); i++ {
			field := val.Field(i)
			if !field.CanSet() {
				continue
			}
			fillExample(field)
		}
		return
	}
	switch val.Kind() {
	case reflect.String:
		val.SetString("example")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		val.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		val.SetUint(1)
	case reflect.Bool:
		val.SetBool(true)
	case reflect.Float32, reflect.Float64:
		val.SetFloat(1.23)
	case reflect.Slice:
		elemType := val.Type().Elem()
		elem := reflect.New(elemType).Elem()
		fillExample(elem)
		val.Set(reflect.MakeSlice(val.Type(), 1, 1))
		val.Index(0).Set(elem)
	case reflect.Map:
		key := reflect.New(val.Type().Key()).Elem()
		fillExample(key)
		value := reflect.New(val.Type().Elem()).Elem()
		fillExample(value)
		m := reflect.MakeMap(val.Type())
		m.SetMapIndex(key, value)
		val.Set(m)
	}
}
