-- TEST-ONLY: lustre_test is routing-check test data, not real metadata. Do not
-- create it on a real upstream; see "Telling them apart" in deploy/README.md.
-- Routing marker for db-a.
--
-- Both upstreams carry this table with the same name and a different row, so
-- "am I actually talking to the server I think I am?" is one query that works
-- identically against either:
--
--   SELECT name FROM lustre_test.upstream_marker;
--
-- A listener wired to the wrong upstream answers 'db-b' here, which is a much
-- clearer failure than a missing table.

CREATE DATABASE IF NOT EXISTS lustre_test;

CREATE TABLE lustre_test.upstream_marker (
  name VARCHAR(32) NOT NULL PRIMARY KEY
);

INSERT INTO lustre_test.upstream_marker (name) VALUES ('db-a');

-- Readable by every account on this server, so the check does not depend on
-- which credentials a listener was configured with.
GRANT SELECT ON lustre_test.* TO 'demo'@'%';
