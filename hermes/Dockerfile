# Pin both supported architectures to the same upstream release.
FROM nousresearch/hermes-agent:v0.23.1@sha256:276d44685c65c0cb2a25f190b63c65c8906f9dc76486fb1c05ae71b5826bda08

ENV HOME=/opt/data PYTHONPATH=/opt/hermes PYTHONDONTWRITEBYTECODE=1
COPY server.py /opt/clanka/hermes_server.py
WORKDIR /tmp
USER 10000:10000
ENTRYPOINT ["/opt/hermes/.venv/bin/python", "/opt/clanka/hermes_server.py"]
CMD []
