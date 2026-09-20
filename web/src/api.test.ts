import { afterEach, describe, expect, it, vi } from 'vitest';

import { ApiError, api, looksLikeYouTubeURL, setCsrfToken } from './api';

function mockFetch(status: number, body: unknown): ReturnType<typeof vi.fn> {
  return vi.fn().mockResolvedValue(
    new Response(body === undefined ? null : JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken('');
});

describe('request wrapper', () => {
  it('parses successful responses', async () => {
    vi.stubGlobal('fetch', mockFetch(200, { jobs: [], total_bytes: 12 }));
    const res = await api.list();
    expect(res.total_bytes).toBe(12);
  });

  it('extracts server error messages', async () => {
    vi.stubGlobal('fetch', mockFetch(400, { error: 'Provide a valid YouTube URL.' }));
    await expect(api.submit('x')).rejects.toMatchObject({ status: 400, message: 'Provide a valid YouTube URL.' });
  });

  it('marks expired sessions as 401 ApiErrors', async () => {
    vi.stubGlobal('fetch', mockFetch(401, { error: 'Authentication required.' }));
    const err = await api.list().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
  });

  it('sends the CSRF token on state changes only', async () => {
    setCsrfToken('token-1');
    const fetchMock = mockFetch(200, { ok: true });
    vi.stubGlobal('fetch', fetchMock);
    await api.remove('abcdefghijk');
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect((init.headers as Record<string, string>)['X-CSRF-Token']).toBe('token-1');

    fetchMock.mockClear();
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ jobs: [] }), { status: 200 }));
    await api.list();
    const listInit = fetchMock.mock.calls[0][1] as RequestInit;
    expect((listInit.headers as Record<string, string>)['X-CSRF-Token']).toBeUndefined();
  });

  it('propagates network failures for the offline banner', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')));
    await expect(api.list()).rejects.toBeInstanceOf(TypeError);
  });

  it('sends the login password and stores nothing on failure', async () => {
    vi.stubGlobal('fetch', mockFetch(401, { error: 'Incorrect password.' }));
    await expect(api.login('wrong')).rejects.toMatchObject({ status: 401 });
  });
});

describe('looksLikeYouTubeURL', () => {
  it('accepts YouTube hosts', () => {
    for (const url of [
      'https://www.youtube.com/watch?v=dQw4w9WgXcQ',
      'https://m.youtube.com/watch?v=dQw4w9WgXcQ',
      'https://music.youtube.com/watch?v=dQw4w9WgXcQ',
      'https://youtu.be/dQw4w9WgXcQ',
      'https://www.youtube.com/shorts/dQw4w9WgXcQ',
    ]) {
      expect(looksLikeYouTubeURL(url), url).toBe(true);
    }
  });

  it('rejects everything else', () => {
    for (const url of [
      '',
      'not a url',
      'https://example.com/watch?v=x',
      'javascript:alert(1)',
      'ftp://youtube.com/x',
      'https://user@youtube.com/watch?v=x',
      'https://evil.com/youtube.com',
    ]) {
      expect(looksLikeYouTubeURL(url), url).toBe(false);
    }
  });
});
