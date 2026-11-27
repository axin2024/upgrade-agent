FROM composer:2.10.0 AS composer
FROM golang:1.26.0-bookworm AS go_toolchain
FROM php:8.2.30-cli-bookworm
COPY --from=composer /usr/bin/composer /opt/upgrade-agent/composer.phar
COPY --from=go_toolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
USER 65534:65534
WORKDIR /work
