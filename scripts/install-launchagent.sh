#!/bin/zsh
# Installs (or reinstalls) a LaunchAgent that starts govee-hvac at login and
# restarts it if it exits. Logs go to ~/Library/Logs/govee-hvac.log.
#
#   zsh scripts/install-launchagent.sh      # build, install, start
#   launchctl kickstart -k gui/$(id -u)/com.jimconning.govee-hvac   # restart after a rebuild
#   launchctl bootout gui/$(id -u)/com.jimconning.govee-hvac        # stop and unload
#   rm ~/Library/LaunchAgents/com.jimconning.govee-hvac.plist       # then remove
set -e
cd "${0:A:h}/.."
APP_DIR=$PWD
LABEL=com.jimconning.govee-hvac
PLIST=$HOME/Library/LaunchAgents/$LABEL.plist
LOG=$HOME/Library/Logs/govee-hvac.log
DOMAIN=gui/$(id -u)

zsh scripts/build.sh

cat > $PLIST <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$APP_DIR/govee-hvac</string>
	</array>
	<key>WorkingDirectory</key>
	<string>$APP_DIR</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>$LOG</string>
	<key>StandardErrorPath</key>
	<string>$LOG</string>
</dict>
</plist>
EOF
plutil -lint $PLIST

# Replace any running copy.
launchctl bootout $DOMAIN/$LABEL 2>/dev/null || true
launchctl bootstrap $DOMAIN $PLIST
echo "Installed $PLIST; logging to $LOG"
