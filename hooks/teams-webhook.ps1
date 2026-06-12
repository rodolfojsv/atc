# atc hook: post session events to a Microsoft Teams incoming webhook.
# Set ATC_TEAMS_WEBHOOK_URL in your environment (or hardcode below).
#
# config.json:
#   "hooks": {
#     "finished": ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:/path/to/hooks/teams-webhook.ps1"]
#   }

$webhookUrl = $env:ATC_TEAMS_WEBHOOK_URL
if (-not $webhookUrl) { exit 0 }

$event = [Console]::In.ReadToEnd() | ConvertFrom-Json

$text = "**atc** — session ``$($event.sessionName)``: $($event.type)"
if ($event.data.summary)  { $text += "`n`n$($event.data.summary)" }
if ($event.data.lastLine) { $text += "`n`n$($event.data.lastLine)" }
if ($event.data.message)  { $text += "`n`n$($event.data.message)" }

$payload = @{ text = $text } | ConvertTo-Json
Invoke-RestMethod -Uri $webhookUrl -Method Post -ContentType "application/json" -Body $payload | Out-Null
