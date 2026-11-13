# The office run profile of idp-sandbox (ADR-0001 D14): the python run image with a pinned, checksum-verified officecli.
# Built by deploy/office/build.sh with --network none; the binary is checked before it reaches this context.
ARG BASE=python:3.12-slim
FROM ${BASE}
COPY officecli /usr/local/bin/officecli
# Runs have no network, a read-only root, HOME=/tmp and a fresh container each time:
#   no update checks and no installs; no resident process (it would die with the run, unflushed), and when an agent opens
#   one anyway (officecli open, which its own references recommend) every change is flushed at once; no .NET diagnostics
#   pipes in /tmp; invariant globalization (the image has no ICU; text of any language is unaffected); no W^X double
#   mapping of JIT code: it sizes a memory file with ftruncate, which the run's file-size limit (the workspace quota) also
#   caps, and .NET aborts (PAL_SEHException) once it is reached (8 MiB measured; ADR-0001 D14).
ENV OFFICECLI_SKIP_UPDATE=1 \
    OFFICECLI_NO_AUTO_INSTALL=1 \
    OFFICECLI_NO_AUTO_RESIDENT=1 \
    OFFICECLI_RESIDENT_FLUSH=each \
    DOTNET_EnableDiagnostics=0 \
    DOTNET_EnableWriteXorExecute=0 \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1
