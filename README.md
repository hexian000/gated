# gated

[![Go Report Card](https://goreportcard.com/badge/github.com/hexian000/gated)](https://goreportcard.com/report/github.com/hexian000/gated)

## What for

gated is used for building HTTP proxy server clusters.

Each peer can announce some host names (which are aliases of some local IP) in a global virtual domain, the cluster will forward the connections to proper peer.

Peers without public IP address can also announce local services. In such case, another peer for reverse proxying will be chosen automatically.

Traffic over untrusted network is carried by multiplexed mTLS tunnels.

```
      Trusted     |   Untrusted   |     Trusted
                              +-------+    +-------+
                         +-1->| gated |-n->| Peer2 |
                         |    +-------+    +-------+
+-------+    +-------+   |    +-------+    +-------+
| Peer1 |-n->| gated |---+-1->| gated |-n->| Peer3 |
+-------+    +-------+   |    +-------+    +-------+
                         |    +-------+    +-------+
                         +-1->| gated |-n->| Peer4 |
                              +-------+    +-------+
```

## Protocol Stack

```
+-------------------------------+
|          HTTP Proxy           |
+-------------------------------+
|   yamux stream multiplexing   |
+-------------------------------+
|        mutual TLS 1.3         |
+-------------------------------+
|  TCP/IP (untrusted network)   |
+-------------------------------+
```


## Authentication Model

Like SSH, each peer needs to generate a key pair(certificate + private key). Only certificates in a peer's authorized certificates list can communicate with this peer.

This behavior is based on golang's mutual TLS 1.3 implementation.

By default, all certificates are self-signed. This will not reduce security. 

## Quick Start

### 1. Generate key pair (or use your own):

```sh
./gencerts.sh peer1 peer2
```

gencerts.sh is in [cmd/gated](cmd/gated/gencerts.sh)

### 2. Create "config.json" per peer

#### Peer1 (with public IP address 203.0.113.1)

```json
{
    "name": "peer1",
    "listen": ":50100",
    "addr": "203.0.113.1:50100",
    "hosts": {
        "server.lan": "127.0.0.1"
    },
    "auth": {
        "cert": "peer1-cert.pem",
        "key": "peer1-key.pem",
        "authcerts": [
            "peer2-cert.pem"
        ]
    }
}
```

#### Peer2 (without public IP address)

```json
{
    "name": "peer2",
    "servers": [
        {
            "addr": "203.0.113.1:50100"
        }
    ],
    "hosts": {
        "router.peer2.lan": "192.168.1.1",
        "peer2.lan": "192.168.1.2"
    },
    "auth": {
        "cert": "peer2-cert.pem",
        "key": "peer2-key.pem",
        "authcerts": [
            "peer1-cert.pem"
        ]
    }
}
```

#### Options

- "name": peer name
- "servers": bootstrap server info
- "servers[\*].addr": bootstrap server address
- "hosts": local hosts
- "hosts[name]": host LAN address
- "cert": peer certificate
- "key": peer private key
- "authcerts": peer authorized certificates list, bundles are supported

see [source code](config/config.go) for complete document

see [config.json](config/config.json) for example config file

### 3. Start

```sh
./gated -c config.json
```

You may also found the systemd user unit [gated.service](gated.service) is useful.

## Build/Install

```sh
# get source code
git clone https://github.com/hexian000/gated.git
cd gated
# build for native system
./make.sh
```
or
```sh
go install github.com/hexian000/gated
```

## Credits

- [go](https://github.com/golang/go)
- [yamux](https://github.com/hashicorp/yamux)
