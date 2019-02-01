## pacroxy


pacproxy is a proxy that parses pac file and start a simple http proxy which will forward http requests to proxies in pac


## Install

Ensure that `Go（>=1.11.5）` enviroment has been properly setup.

```bash
go get -u -v github.com/darren/pacroxy
```


## Usage

```
pacroxy -p wpad.dat -l 127.0.0.1:9999
curl -x 127.0.0.1:9999 https://example.com
```

## Note

This is a simple tool still in development, use at your own risk.


