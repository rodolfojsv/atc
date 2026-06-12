# atc hook: trigger an Obsidian LiveSync replication right after an
# atc event (typically "finished", paired with autoExport), so exported
# session notes reach CouchDB — and your phone — immediately.
#
# Requires on this machine:
#   - Obsidian with your vault open (the URI launches it if closed)
#   - the "Advanced URI" community plugin (executes commands via URI)
#   - Self-hosted LiveSync configured
#
# Environment:
#   ATC_OBSIDIAN_VAULT    vault name as shown in Obsidian (required)
#   ATC_OBSIDIAN_COMMAND  command id to run; default is LiveSync's
#                         replicate command. To find/verify the id, use
#                         Advanced URI's "Copy URI for command" from the
#                         command palette on "Self-hosted LiveSync: Replicate now".
#
# config.json:
#   "hooks": {
#     "finished": ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:/dev/atc/hooks/obsidian-sync.ps1"]
#   }

$null = [Console]::In.ReadToEnd()   # consume the event JSON; not needed here

$vault = $env:ATC_OBSIDIAN_VAULT
if (-not $vault) { exit 0 }

$command = $env:ATC_OBSIDIAN_COMMAND
if (-not $command) { $command = "obsidian-livesync:livesync-replicate" }

$uri = "obsidian://advanced-uri?vault=" + [Uri]::EscapeDataString($vault) +
       "&commandid=" + [Uri]::EscapeDataString($command)
Start-Process $uri
