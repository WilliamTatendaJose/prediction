#!/bin/sh
# Runs once, when the Postgres volume is first created, as the superuser
# (POSTGRES_USER). Creates least-privilege roles:
#   iothub      owns the iothub database and schema (the hub connects as it;
#               not a superuser)
#   grafana_ro  read-only: a reporting tool must not be able to change data
# The hub creates its tables later as iothub; default privileges give
# grafana_ro SELECT on them automatically.
set -e
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<SQL
CREATE ROLE iothub LOGIN PASSWORD '${IOTHUB_DB_PASSWORD}';
CREATE ROLE grafana_ro LOGIN PASSWORD '${GRAFANA_DB_PASSWORD}';
ALTER DATABASE "$POSTGRES_DB" OWNER TO iothub;
ALTER SCHEMA public OWNER TO iothub;
REVOKE ALL ON DATABASE "$POSTGRES_DB" FROM PUBLIC;
GRANT CONNECT ON DATABASE "$POSTGRES_DB" TO iothub, grafana_ro;
GRANT USAGE ON SCHEMA public TO grafana_ro;
ALTER DEFAULT PRIVILEGES FOR ROLE iothub IN SCHEMA public GRANT SELECT ON TABLES TO grafana_ro;
ALTER ROLE grafana_ro SET statement_timeout = '30s';
ALTER ROLE grafana_ro SET default_transaction_read_only = on;
SQL
