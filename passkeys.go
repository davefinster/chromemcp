package main

// Passkeys and security keys, hidden on demand. A sign-in that asks for one
// hands the request to the browser's own WebAuthn dialog -- a native window
// that no live view can show, and that on a Mac node takes the page's input
// while it waits (seen at Google's 2-Step Verification: the page could not
// be clicked, "Try another way" included). A session started with
// disable_passkeys has no WebAuthn as far as its pages can tell: no
// PublicKeyCredential, and navigator.credentials refusing any publicKey
// request at once, as a browser with no authenticator would. A site then
// offers its other methods -- a phone prompt, a code -- and nothing native
// ever opens. Password credentials are untouched.
//
// It is opt-in and per session: a Chrome without WebAuthn is unusual, and
// that is itself something a site can see.

// noPasskeysScript runs in every document before the page's own scripts,
// beside the device profile's (emulate.go applies both).
const noPasskeysScript = `(() => {
  'use strict';
  if (typeof window === 'undefined') return;
  try { delete window.PublicKeyCredential; } catch (e) {}
  try { if ('PublicKeyCredential' in window) Object.defineProperty(window, 'PublicKeyCredential', {value: undefined, configurable: true, writable: true}); } catch (e) {}
  if (typeof CredentialsContainer === 'undefined') return;
  const proto = CredentialsContainer.prototype;
  for (const name of ['get', 'create']) {
    const orig = proto[name];
    if (typeof orig !== 'function') continue;
    const wrapped = {[name](options) {
      if (options && options.publicKey) {
        return Promise.reject(new DOMException('The operation either timed out or was not allowed.', 'NotAllowedError'));
      }
      return orig.apply(this, arguments);
    }}[name];
    Object.defineProperty(wrapped, 'toString', {value: () => 'function ' + name + '() { [native code] }'});
    Object.defineProperty(proto, name, {value: wrapped, writable: true, configurable: true, enumerable: true});
  }
})();`
