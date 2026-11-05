# craggy collector sidecar: tcpdump for net.pcap, inotify-tools for
# fs.activity. The sidecar gets the extra capabilities (NET_RAW) and
# shares the target's namespaces; the target never does.
FROM alpine:3.20
RUN apk add --no-cache tcpdump inotify-tools procps
