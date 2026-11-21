#!/bin/zsh
# Builds ./govee-hvac with packaging/Info.plist embedded (needed for Bluetooth
# when the app runs outside a terminal, e.g. as the login agent).
#
# Note: each rebuild changes the binary's signature, so macOS may ask again
# for Bluetooth permission the next time it starts.
set -e
cd "${0:A:h}/.."
go build \
  -ldflags "-linkmode external -extldflags '-sectcreate __TEXT __info_plist $PWD/packaging/Info.plist'" \
  -o govee-hvac .
# Re-sign ad hoc under the bundle ID so macOS privacy settings show a real name
# (the linker's own signature calls it "a.out").
codesign --force --sign - --identifier com.jimconning.govee-hvac govee-hvac
echo "Built $PWD/govee-hvac"
