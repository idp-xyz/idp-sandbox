FROM scratch
COPY idp-sandbox /idp-sandbox
# the Docker socket is root's: the sandbox runs as root in its own container (a development topology; see
# docs/adr/ADR-0001-sandbox-design.md for the production placement), everything it starts runs as 65534
USER 0:0
ENTRYPOINT ["/idp-sandbox"]
