# The Hub image (docs/spec/hub.md §2.6). The binary is built outside the
# image: GoReleaser (dockers_v2) puts each platform's prebuilt binary under
# $TARGETPLATFORM/ in the build context, and `just image` does the same
# locally.
FROM alpine:3.22

ARG TARGETPLATFORM

# sqlite for poking at hub.db with the sqlite3 shell; tzdata so TZ works.
RUN apk add --no-cache sqlite ca-certificates tzdata \
	&& addgroup -g 1000 hub \
	&& adduser -D -H -u 1000 -G hub hub \
	&& mkdir /data /backups \
	&& chown 1000:1000 /data /backups

COPY $TARGETPLATFORM/agent-history-hub /usr/local/bin/agent-history-hub

USER 1000:1000
VOLUME ["/data", "/backups"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --start-interval=2s \
	CMD ["agent-history-hub", "healthcheck"]

ENTRYPOINT ["agent-history-hub"]
CMD ["serve"]
