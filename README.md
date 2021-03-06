## docker-distribution-postgresql [![Build Status](https://travis-ci.org/noxiouz/docker-distribution-postgresql.svg?branch=master)](https://travis-ci.org/noxiouz/docker-distribution-postgresql) [![codecov.io](https://codecov.io/github/noxiouz/docker-distribution-postgresql/coverage.svg?branch=master)](https://codecov.io/github/noxiouz/docker-distribution-postgresql?branch=master)
This driver stores metadata for files in PostgreSQL and binary data in a KV storage.

### Configuration

```yaml
storage:
    postgres:
        URLs:
          - "postgres://noxiouz@localhost:5432/distribution?sslmode=disable"
        MaxOpenConns: 10
        MaxIdleConns: 5
        type: "mds"
        options:
            host: "mdshost.yandex.net"
            uploadport: 1111
            readport: 80
            authheader: "Basic <basic auth header>"
            namespace: "some-namepace"
```

### KV Backends

 + **inmemory** - just for tests
 + **mds** - for Yandex internal purposes
 + **elliptics** - TBD

### Status

The driver is working in production at Yandex
