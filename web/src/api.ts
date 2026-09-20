// API client: one thin fetch wrapper plus typed endpoints. The CSRF token is
// kept in module state and attached to every state-changing request.

export interface JobView {
  id: string;
  title: string;
  status: string;
  stage: string;
  progress?: number | null;
  size_bytes: number;
  created_at: string;
  error?: string;
}

export interface ListResponse {
  jobs: JobView[];
  total_bytes: number;
}

export interface SubmitResponse {
  job: JobView;
  duplicate: boolean;
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

let csrfToken = '';

export function setCsrfToken(token: string): void {
  csrfToken = token;
}

export function currentCsrfToken(): string {
  return csrfToken;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && method !== 'HEAD' && csrfToken) headers['X-CSRF-Token'] = csrfToken;
  let res: Response;
  try {
    res = await fetch(path, { method, credentials: 'same-origin', headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch (err) {
    // Network failure: the caller shows the offline banner and retries.
    throw err;
  }
  let data: unknown = null;
  try {
    data = await res.json();
  } catch {
    // Empty body (e.g. 204-style responses); leave data null.
  }
  if (!res.ok) {
    const message = typeof data === 'object' && data !== null && 'error' in data && typeof (data as { error: unknown }).error === 'string'
      ? (data as { error: string }).error
      : `Request failed (${res.status}).`;
    throw new ApiError(res.status, message);
  }
  return data as T;
}

export const api = {
  login: (password: string) => request<{ csrf_token: string }>('POST', '/api/login', { password }),
  session: () => request<{ csrf_token: string }>('GET', '/api/session'),
  logout: () => request<{ ok: boolean }>('POST', '/api/logout'),
  list: () => request<ListResponse>('GET', '/api/jobs'),
  submit: (url: string) => request<SubmitResponse>('POST', '/api/jobs', { url }),
  retry: (id: string) => request<{ job: JobView }>('POST', `/api/jobs/${id}/retry`),
  remove: (id: string) => request<{ ok: boolean }>('DELETE', `/api/jobs/${id}`),
  shareLink: (id: string) => request<{ url: string; expires_at: string }>('POST', `/api/jobs/${id}/link`),
  downloadURL: (id: string) => `/api/jobs/${id}/download`,
};

const youtubeHosts = new Set(['youtube.com', 'www.youtube.com', 'm.youtube.com', 'music.youtube.com', 'youtu.be']);

// looksLikeYouTubeURL is a client-side pre-check so the modal can reject
// non-YouTube input without a round trip; the server remains the authority.
export function looksLikeYouTubeURL(raw: string): boolean {
  let url: URL;
  try {
    url = new URL(raw.trim());
  } catch {
    return false;
  }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') return false;
  if (!youtubeHosts.has(url.hostname.toLowerCase())) return false;
  if (url.username || url.password) return false;
  return true;
}
