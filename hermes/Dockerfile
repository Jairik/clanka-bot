# Pin both supported architectures to the current stable release.
# v0.23.3 matches :stable and includes the v0.23.2 security fix.
FROM nousresearch/hermes-agent:v0.23.3@sha256:3e9fe4c6ef36490f43aa0d0d80f9df64e9c92a576eee3042f1a97c8813c05355

ENV HOME=/opt/data PYTHONPATH=/opt/hermes PYTHONDONTWRITEBYTECODE=1
# Keep the historical entrypoint path so a live overlay can bind-mount over it.
COPY server.py /opt/clanka/hermes_server.py
COPY sample_mcp.py /opt/clanka/sample_mcp.py
# Hermes starts the Go adapter client as a lazy stdio child.
COPY go_mcp.py /opt/clanka/go_mcp.py
WORKDIR /tmp
USER 10000:10000
ENTRYPOINT ["/opt/hermes/.venv/bin/python", "/opt/clanka/hermes_server.py"]
CMD []
