# Lantern Box

Lantern Box is a censorship circumvention proxy and client platform built on
[sing-box](https://github.com/SagerNet/sing-box), with additional protocols from
Lantern.

See the [project documentation](https://github.com/getlantern/lantern-box#readme)
for supported protocols, configuration examples, and deployment guidance.

## Image tags

- `getlantern/lantern-box:latest`: the latest published release image.
- `getlantern/lantern-box:vX.Y.Z`: a specific release.

The release image supports Linux AMD64.

## Run

Create a `config.json` using the project documentation, then mount it at
`/config.json`. For example, for a server listening on TCP and UDP port 443:

```sh
docker run -d --name lantern-box \
  --restart unless-stopped \
  -p 443:443/tcp \
  -p 443:443/udp \
  --mount type=bind,src="$(pwd)/config.json",dst=/config.json,readonly \
  getlantern/lantern-box:latest
```

Adjust the published ports to match your configured listeners. Mount any
certificates, keys, or other files referenced by the configuration at their
configured container paths.

The default command is `lantern-box run --config /config.json`.

## Source

- [Source code and issues](https://github.com/getlantern/lantern-box)
- [Releases](https://github.com/getlantern/lantern-box/releases)
