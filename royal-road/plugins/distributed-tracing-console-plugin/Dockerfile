# openshift/distributed-tracing-console-plugin, frontend only, built from
# upstream source with public base images. Upstream's image also contains a Go
# backend that discovers TempoStack/TempoMonolithic resources and proxies to
# them; here royal-road/trace-federator serves that backend API instead (the
# bridge's plugin proxy points at it), so only the static bundle is built:
#
#   docker build -t distributed-tracing-console-plugin royal-road/plugins/distributed-tracing-console-plugin

FROM docker.io/library/node:22 AS build

WORKDIR /src
COPY upstream.env /tmp/upstream.env
COPY patches/ /tmp/patches/

# Fetch exactly the pinned commit, then apply our patches in order.
RUN . /tmp/upstream.env && \
    git init -q . && \
    git fetch -q --depth 1 "$UPSTREAM_REPO" "$UPSTREAM_COMMIT" && \
    git -c advice.detachedHead=false checkout -q FETCH_HEAD && \
    for p in /tmp/patches/*.patch; do [ -e "$p" ] || continue; echo "applying $p"; git apply "$p"; done

# Same steps as upstream's Makefile (install-frontend-ci, build-frontend).
ENV HUSKY=0
RUN cd web && \
    npm ci --ignore-scripts --omit=optional --no-audit --no-fund && \
    npm run build

# UBI nginx (public, no credentials). Its default config serves
# /opt/app-root/src over plain HTTP on 8080 as UID 1001; the bridge fetches the
# static assets and proxies them to the browser.
FROM registry.access.redhat.com/ubi9/nginx-126

COPY --from=build /src/web/dist /opt/app-root/src

USER 1001
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]

LABEL \
        org.opencontainers.image.source="https://github.com/royal-road/console" \
        org.opencontainers.image.description="openshift/distributed-tracing-console-plugin built by the Royal Road console fork" \
        org.opencontainers.image.licenses="Apache-2.0"
