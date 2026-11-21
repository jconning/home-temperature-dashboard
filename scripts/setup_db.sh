#!/bin/zsh
# Generates the MySQL setup SQL and the app's DSN file.
#
#   zsh scripts/setup_db.sh                    # writes config/db_setup.sql + config/db_dsn
#   mysql -u root -p < config/db_setup.sql     # run once as a MySQL admin
#
# The password is generated once and kept in config/db_password, so re-running
# this script is safe and keeps the same credentials.
set -e
cd "${0:A:h}/.."

DB=govee_thermo
DBUSER=govee

umask 077
mkdir -p config
if [[ ! -s config/db_password ]]; then
  # Fixed prefix satisfies validate_password (upper, lower, digit, special);
  # the hex supplies the entropy and keeps the DSN URL-safe.
  print -r -- "Gv-1A$(openssl rand -hex 16)" > config/db_password
fi
PW=$(tr -d '[:space:]' < config/db_password)

cat > config/db_setup.sql <<EOF
CREATE DATABASE IF NOT EXISTS \`$DB\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE USER IF NOT EXISTS '$DBUSER'@'%' IDENTIFIED BY '$PW';
ALTER USER '$DBUSER'@'%' IDENTIFIED BY '$PW';
GRANT ALL PRIVILEGES ON \`$DB\`.* TO '$DBUSER'@'%';
FLUSH PRIVILEGES;
EOF

print -r -- "$DBUSER:$PW@tcp(127.0.0.1:3306)/$DB?parseTime=true&loc=UTC&time_zone=%27%2B00%3A00%27&charset=utf8mb4" > config/db_dsn

echo "Wrote config/db_setup.sql and config/db_dsn"
echo "Now run:  mysql -u root -p < config/db_setup.sql"
