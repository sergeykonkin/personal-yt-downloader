// main.ts bootstraps on import, so this drives the whole boot flow: the DOM
// and fetch stubs go up before the dynamic import, and the module's own
// polling picks the stubs up.

import { afterEach, describe, expect, it, vi } from 'vitest';

import { api, currentCsrfToken, setCsrfToken } from './api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken('');
});

describe('boot', () => {
  it('recovers the CSRF token through polling after the boot session check fails by network', async () => {
    document.body.innerHTML = '<div id="app"></div>';

    let sessionCalls = 0;
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/session') {
        sessionCalls += 1;
        // The boot check dies by network; the poll's re-check succeeds.
        if (sessionCalls === 1) return Promise.reject(new TypeError('Failed to fetch'));
        return Promise.resolve(jsonResponse({ csrf_token: 'token-1' }));
      }
      if (url === '/api/jobs' && method === 'GET') {
        return Promise.resolve(jsonResponse({ jobs: [], total_bytes: 0 }));
      }
      if (url.startsWith('/api/jobs/') && method === 'DELETE') {
        return Promise.resolve(jsonResponse({ ok: true }));
      }
      return Promise.resolve(jsonResponse({ error: 'unexpected request' }, 500));
    });
    vi.stubGlobal('fetch', fetchMock);

    await import('./main'); // boot() runs; the session check fails by network,
    // the offline banner shows, and polling starts.

    // The poll's refresh() re-checks the session and stores the token.
    await vi.waitFor(() => {
      expect(currentCsrfToken()).toBe('token-1');
    });
    // ...and the recovered poll clears the offline banner.
    await vi.waitFor(() => {
      expect((document.querySelector('.offline') as HTMLElement).hidden).toBe(true);
    });

    // The recovered token protects state-changing requests again.
    await api.remove('abcdefghijk');
    const deleteCall = fetchMock.mock.calls.find(([, init]) => init?.method === 'DELETE');
    expect(deleteCall).toBeDefined();
    const headers = (deleteCall![1] as RequestInit).headers as Record<string, string>;
    expect(headers['X-CSRF-Token']).toBe('token-1');
  });
});
