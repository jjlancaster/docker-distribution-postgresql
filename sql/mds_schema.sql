CREATE TABLE mds (
    KEY 	TEXT PRIMARY KEY,
    MDSFILEINFO TEXT NOT NULL,
    DELETED BOOLEAN NOT NULL DEFAULT FALSE
);
