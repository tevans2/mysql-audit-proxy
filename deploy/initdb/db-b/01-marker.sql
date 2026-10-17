-- TEST-ONLY: lustre_test is routing-check test data, not real metadata. Do not
-- create it on a real upstream; see "Telling them apart" in deploy/README.md.
-- Routing marker for db-b. See deploy/initdb/db-a/01-marker.sql — same table,
-- different row, so the same query distinguishes the two upstreams.

CREATE DATABASE IF NOT EXISTS lustre_test;

CREATE TABLE lustre_test.upstream_marker (
  name VARCHAR(32) NOT NULL PRIMARY KEY
);

INSERT INTO lustre_test.upstream_marker (name) VALUES ('db-b');

GRANT SELECT ON lustre_test.* TO 'hr_app'@'%';
