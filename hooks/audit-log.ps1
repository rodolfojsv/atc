# atc hook: append every event it receives as a JSON line to an audit log.
# Useful on "tool-call" to keep a record of everything agents executed.
#
# config.json:
#   "hooks": {
#     "tool-call": ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:/path/to/hooks/audit-log.ps1"]
#   }

$line = [Console]::In.ReadToEnd().Trim()
$logDir = Join-Path $env:USERPROFILE ".atc"
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
Add-Content -Path (Join-Path $logDir "audit.jsonl") -Value $line
