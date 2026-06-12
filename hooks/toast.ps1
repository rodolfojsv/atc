# atc hook: Windows toast notification.
# Reads the atc event as JSON from stdin and shows a native toast.
#
# config.json:
#   "hooks": {
#     "waiting-on-permission": ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:/path/to/hooks/toast.ps1"],
#     "finished":              ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:/path/to/hooks/toast.ps1"]
#   }

$event = [Console]::In.ReadToEnd() | ConvertFrom-Json

switch ($event.type) {
    "waiting-on-permission" { $title = "atc: $($event.sessionName) is blocked" ; $body = $event.data.summary }
    "finished"              { $title = "atc: $($event.sessionName) finished"   ; $body = $event.data.lastLine }
    "error"                 { $title = "atc: $($event.sessionName) error"      ; $body = $event.data.message }
    default                 { $title = "atc: $($event.sessionName) — $($event.type)" ; $body = "" }
}

[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null

$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent(
    [Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$texts = $xml.GetElementsByTagName("text")
$texts.Item(0).AppendChild($xml.CreateTextNode($title)) | Out-Null
$texts.Item(1).AppendChild($xml.CreateTextNode([string]$body)) | Out-Null

$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)
# Borrow PowerShell's app identity so no AppX registration is needed.
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier(
    "{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe").Show($toast)
