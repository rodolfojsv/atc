// atc service worker.
//
// Its whole reason to exist (for now) is notifications: Android Chrome
// forbids the `new Notification()` constructor and only allows
// ServiceWorkerRegistration.showNotification(), so the "bell" silently
// did nothing on phones. With this worker registered, the page calls
// registration.showNotification() instead and alerts work on Android,
// desktop, and an installed PWA alike.
//
// It also makes a tapped notification focus the existing app window (or
// open one), and — when an action button is used — tells the page which
// session/decision was chosen so it can hit /respond. The page posts a
// message back so we keep no atc state or token in the worker itself.

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const data = event.notification.data || {};
  event.waitUntil((async () => {
    const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const client = clients.find((c) => 'focus' in c);
    const payload = { type: 'notification-action', action: event.action || 'open', data };
    if (client) {
      await client.focus();
      client.postMessage(payload);
      return;
    }
    if (self.clients.openWindow) {
      // No live window: open the board. The action is dropped (the page
      // isn't there to act on it) but at least the app comes up.
      await self.clients.openWindow('/');
    }
  })());
});
