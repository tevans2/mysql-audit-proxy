-- db-b's own schema: 'hr'.
--
-- Deliberately shares no table names with db-a's 'sales', so a query routed to
-- the wrong upstream errors rather than returning believable rows.

CREATE TABLE hr.departments (
  id   INT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL
);

CREATE TABLE hr.employees (
  id            INT AUTO_INCREMENT PRIMARY KEY,
  department_id INT NOT NULL,
  full_name     VARCHAR(100) NOT NULL,
  title         VARCHAR(100) NOT NULL,
  started_on    DATE NOT NULL,
  CONSTRAINT fk_employees_department FOREIGN KEY (department_id) REFERENCES hr.departments (id)
);

INSERT INTO hr.departments (name) VALUES
  ('Engineering'),
  ('Finance');

INSERT INTO hr.employees (department_id, full_name, title, started_on) VALUES
  (1, 'N. Petersen',  'Platform Engineer', '2023-02-01'),
  (1, 'T. Molefe',    'Database Engineer', '2024-07-15'),
  (2, 'S. van Wyk',   'Financial Analyst', '2022-11-03');

GRANT SELECT, INSERT, UPDATE, DELETE ON hr.* TO 'hr_app'@'%';
