# printkeeper launchd agent

`io.github.phpfc.printkeeper.plist` is a **template**: launchd cannot expand
`~` or `$HOME` inside `ProgramArguments`, `StandardOutPath`, or
`StandardErrorPath`, so every `__HOME__` placeholder in the file must be
substituted with a real, absolute home directory before the plist is handed
to `launchctl`.

## Install

Build the daemon, put it where the plist expects it, substitute the
placeholder, and bootstrap it for the current user:

```sh
go build -o "$HOME/Library/Application Support/printkeeper/bin/printkeeperd" ./cmd/printkeeperd
mkdir -p "$HOME/Library/Logs/printkeeper"

sed "s#__HOME__#$HOME#g" packaging/darwin/io.github.phpfc.printkeeper.plist \
  > /tmp/io.github.phpfc.printkeeper.plist

launchctl bootstrap gui/$(id -u) /tmp/io.github.phpfc.printkeeper.plist
```

## Verify

```sh
launchctl list | grep io.github.phpfc.printkeeper
printkeeper doctor   # answers over the daemon's socket if it's running
```

## Remove

```sh
launchctl bootout gui/$(id -u)/io.github.phpfc.printkeeper
```

## Why a template instead of a fixed path

A committed plist with one developer's own absolute home directory baked in
only works on that one machine — a non-functional artifact for anyone else
who checks this repository out. `packaging/packaging_test.go` enforces this
mechanically: it fails `go test ./...` if a literal macOS per-user home path
is ever committed anywhere under `packaging/` again.
