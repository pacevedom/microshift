# {{- if env.Getenv "CURRENT_RELEASE_VERSION" "" -}}
# Note: This comment makes templating add a new line before the code
# Inherit from crel which already has mandatory RPMs, firewalld, and firewall config
FROM localhost/rhel100-bootc-crel:latest

# Copy repository configuration
COPY ./bootc-images/microshift-fast-datapath-rhel9.repo ./bootc-images/microshift-crel.repo ./bootc-images/microshift-rhocp-y.repo \
    /etc/yum.repos.d/

# Print repository configuration contents.
# Install MicroShift optional packages only (mandatory already installed in parent).
RUN dnf repoinfo --enabled && \
    dnf install -y \
        {{ range (env.Getenv "MICROSHIFT_OPTIONAL_RPMS" | strings.Split " ") -}}
        "{{ . }}-{{ env.Getenv "CURRENT_RELEASE_VERSION" }}" \
        {{ end -}}
        {{ if and (env.Getenv "UNAME_M" "") (eq "x86_64" .Env.UNAME_M) -}}
        {{ range (env.Getenv "MICROSHIFT_X86_64_RPMS" | strings.Split " ") -}}
        "{{ . }}-{{ env.Getenv "CURRENT_RELEASE_VERSION" }}" \
        {{ end -}}
        {{ end -}}
        && \
    rm -vf /etc/yum.repos.d/microshift-*.repo && \
    dnf clean all
# {{- end -}}
