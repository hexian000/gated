# gated

[![Go Report Card](https://goreportcard.com/badge/github.com/hexian000/gated)](https://goreportcard.com/report/github.com/hexian000/gated)

## What for

gated is a HTTP proxy server which can place multiple hosts in different LANs in a same virtual domain. I personally use it to easily and securely access any service on my router/raspberry pi/server in different locations.

Each peer can publish some virtual host names (which are aliases of some local IP addresses or real host names) in a global virtual domain, then any peer will be able to connect to this host via the virtual host name.

When a network failure occurs, some peers can't be directly connected, and gated may automatically choose a third peer that can reach both peers at the same time to forward the traffic.

Peers without public IP address can also publish local services. In this case, a reverse proxy will be performed automatically.

For now, this tool is only designed to handle small clusters.

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

### 1. Generate new self-signed certificates with OpenSSL (or you may use existing ones):

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
    "httplisten": "192.168.123.1:8080",
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
    "httplisten": ":8080",
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
- "listen": listen address for other peers
- "addr": advertised address for other peers to connect
- "httplisten": listen address of the local HTTP proxy
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


### 4. Use

Check http://api.gated.lan/status over the HTTP proxy for service status.

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
go install github.com/hexian000/gated/cmd/gated@latest
```

## Credits

- [go](https://github.com/golang/go)
- [yamux](https://github.com/hashicorp/yamux)
