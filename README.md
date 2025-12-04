# Docker NS1 Dynamic DNS
This updates DNS records in NS1 with the current IP every 5 minutes. The script runs under `cron` inside a lightweight Alpine-based Docker container.

~[NS1](https://ns1.com) is a DNS provider that offers a generous free plan (500k queries/month, 50 records) and an API.~

IBM has since purchased NS1 and it appears that the free plan is now only a free trial.

### IP Sources
In external mode, one of three sources will be picked randomly:
* [ipify.org](https://www.ipify.org)
* [ipinfo.io](https://ipinfo.io)
* [ifconfig.co](https://ifconfig.co)


## Build
To build the go application and set up the container, run:
```
docker compose build
```

## Usage
### docker-compose
```yaml
services:
  dynamic-dns:
    environment:
      - FREQUENCY=5
    image: stevennic22/ns1-dynamic-dns:latest
    volumes:
      - /your/config.yml:/app/config/config.yml:ro
    restart: unless-stopped
```

```
docker compose up -d [--build]
```

The build parameter is optional. If the image has not been built yet, it will build on the first run.

### docker run
```
docker run -d \
    -v /your/config.yml:/app/config/config.yml:ro \
    --env FREQUENCY=5 \
    --name=ddns
    ns1-dynamic-dns:latest
```

### Custom frequency
You can change the value of the `FREQUENCY` environment variable to make the script run every `$FREQUENCY` minutes. The default is every 5 minutes.

### Testing
To test the script, run it through `docker run` and append `/app/ns1-dynamic-dns`. This will run the script once, then kill the container. Example:

```
docker run --rm -v /your/config.yml:/app/config/config.yml:ro stevennic22/ns1-dynamic-dns:latest /app/ns1-dynamic-dns
```

## Config file
A `config.yml` file **must** be passed or the container won't be able to do anything. The format for the config file can be seen in `example-config.yml`.


## Why
Based off of https://github.com/stevennic22/docker-ns1-dynamic-dns, which was originally forked to add Linode host management. After running into some issues with updating the previous version, opted for a Golang approach to hopefully make future updates easier.
