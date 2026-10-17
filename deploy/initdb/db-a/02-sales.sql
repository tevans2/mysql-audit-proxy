-- db-a's own schema: 'sales'.
--
-- The point of the two-upstream environment is that the databases differ. A
-- query written against 'sales' fails outright on db-b, so a routing mistake
-- surfaces as an error rather than as plausible-looking rows.
--
-- The 'demo' database and user the entrypoint creates from MYSQL_DATABASE /
-- MYSQL_USER are left alone: internal/proxy/runtime's integration tests connect to
-- 'demo' as 'demo'/'demopass' and would break if this server stopped
-- providing them.

CREATE DATABASE IF NOT EXISTS sales;

CREATE TABLE sales.customers (
  id         INT AUTO_INCREMENT PRIMARY KEY,
  name       VARCHAR(100) NOT NULL,
  email      VARCHAR(200) NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sales.orders (
  id          INT AUTO_INCREMENT PRIMARY KEY,
  customer_id INT NOT NULL,
  total_cents INT NOT NULL,
  placed_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES sales.customers (id)
);

INSERT INTO sales.customers (name, email) VALUES
  ('Example Software', 'accounts@example-software.example'),
  ('Cape Fitters',    'ops@capefitters.example'),
  ('Bothma Logistics','admin@bothma.example');

INSERT INTO sales.orders (customer_id, total_cents) VALUES
  (1, 249900),
  (1,  81500),
  (2, 129950),
  (3,  45000);

-- Per-upstream application account. Distinct from db-b's, so a listener
-- configured with the wrong upstream's credentials fails to authenticate
-- instead of quietly connecting to the wrong server.
CREATE USER 'sales_app'@'%' IDENTIFIED BY 'salespw';
GRANT SELECT, INSERT, UPDATE, DELETE ON sales.* TO 'sales_app'@'%';
GRANT SELECT ON lustre_test.* TO 'sales_app'@'%';
