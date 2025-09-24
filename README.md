# OSS Index Caching Proxy

## 1. Purpose

Can be used by organizations to centralize outgoing calls to OSS SonaType public repository.
Inspired by https://github.com/dependency-check/DependencyCheck/issues/7937,
and https://ossindex.sonatype.org/doc/auth-required.

See `sample_config.yaml` for a sample configuration file.

Authenticates incoming requests using GitLab PAT tokens (`read_user` scope is sufficient)

Does not offer SSL, expected to be put behind NGINX or similar reverse-proxy

