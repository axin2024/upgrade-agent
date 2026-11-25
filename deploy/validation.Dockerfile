FROM composer:2.10.0 AS composer
FROM php:8.2.30-cli-bookworm
COPY --from=composer /usr/bin/composer /opt/upgrade-agent/composer.phar
USER 65534:65534
WORKDIR /work
