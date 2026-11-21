CREATE TABLE IF NOT EXISTS devices (
  device_id  VARCHAR(64)  NOT NULL PRIMARY KEY,
  sku        VARCHAR(32)  NOT NULL,
  name       VARCHAR(128) NOT NULL,
  temp_offset_f DECIMAL(5,2) NOT NULL DEFAULT 0 COMMENT 'Govee app calibration, applied to raw Bluetooth readings',
  hum_offset    DECIMAL(4,1) NOT NULL DEFAULT 0,
  ble_address   VARCHAR(64)  NULL COMMENT 'this computer''s Bluetooth identifier for the sensor',
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS readings (
  device_id VARCHAR(64)  NOT NULL,
  ts        DATETIME     NOT NULL COMMENT 'UTC',
  temp_f    DECIMAL(6,2) NOT NULL,
  humidity  DECIMAL(5,2) NULL,
  source    ENUM('api','import','ble') NOT NULL DEFAULT 'api',
  PRIMARY KEY (device_id, ts),
  KEY idx_readings_ts (ts),
  CONSTRAINT fk_readings_device FOREIGN KEY (device_id) REFERENCES devices (device_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS poll_log (
  id             BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
  started_at     DATETIME(3) NOT NULL COMMENT 'UTC',
  duration_ms    INT         NOT NULL,
  devices_ok     INT         NOT NULL,
  devices_failed INT         NOT NULL,
  error          TEXT        NULL,
  KEY idx_poll_log_started (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
