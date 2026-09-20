// Bootstrap: session check, polling loop, and the flows between API, cards,
// and modals.

import { ApiError, api, currentCsrfToken, setCsrfToken, type JobView } from './api';
import { confirmDeleteModal, loginModal, submitModal } from './modals';
import {
  createList,
  highlightCard,
  initApp,
  setOffline,
  setTotal,
  showToast,
  type AppElements,
  type ListController,
} from './ui';
import './style.css';

const POLL_INTERVAL_MS = 2000;

let elements: AppElements;
let list: ListController;
let pollTimer = 0;
let refreshing = false;

// A network failure at boot can leave the app polling without a CSRF token,
// which would 403 every state-changing request until a reload. Each poll
// re-checks the session, so the token lands as soon as the connection
// recovers; a 401 here means the session itself is gone and lands in the
// same catch as the list request would.
async function ensureSession(): Promise<void> {
  if (currentCsrfToken() === '') {
    const { csrf_token } = await api.session();
    setCsrfToken(csrf_token);
  }
}

async function refresh(): Promise<void> {
  if (refreshing) return;
  refreshing = true;
  try {
    await ensureSession();
    const { jobs, total_bytes } = await api.list();
    setOffline(elements, false);
    list.sync(jobs);
    setTotal(elements, total_bytes);
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      stopPolling();
      showLogin();
    } else {
      setOffline(elements, true);
    }
  } finally {
    refreshing = false;
  }
}

function startPolling(): void {
  stopPolling();
  void refresh();
  pollTimer = window.setInterval(() => void refresh(), POLL_INTERVAL_MS);
}

function stopPolling(): void {
  if (pollTimer) {
    window.clearInterval(pollTimer);
    pollTimer = 0;
  }
}

// Pausing while hidden and refreshing on return keeps the update cadence tied
// to what the user actually sees.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') {
    void refresh();
  }
});

function showLogin(): void {
  if (document.querySelector('.overlay-login')) return;
  loginModal(async (password) => {
    if (password === '') return 'Enter the password.';
    try {
      const { csrf_token } = await api.login(password);
      setCsrfToken(csrf_token);
      startPolling();
      return null;
    } catch (err) {
      if (err instanceof ApiError && err.status === 429) return 'Too many attempts. Try again later.';
      if (err instanceof ApiError && err.status === 401) return 'Incorrect password.';
      return 'Could not sign in. Check your connection.';
    }
  });
}

function showSubmit(): void {
  submitModal(async (url) => {
    try {
      const { job, duplicate } = await api.submit(url);
      await refresh();
      if (duplicate) {
        // The server named the existing card: draw attention to it.
        const card = list.cardFor(job.id);
        if (card) highlightCard(card);
        return 'duplicate';
      }
      return 'ok';
    } catch (err) {
      if (err instanceof ApiError && err.status === 429) {
        return { error: 'The queue is full. Wait for a video to finish.' };
      }
      if (err instanceof ApiError) return { error: err.message };
      return { error: 'Could not submit. Check your connection.' };
    }
  });
}

function confirmDelete(job: JobView): void {
  confirmDeleteModal(job.title, async () => {
    try {
      await api.remove(job.id);
      await refresh();
      return null;
    } catch (err) {
      if (err instanceof ApiError) return err.message;
      return 'Could not delete. Check your connection.';
    }
  });
}

async function copyLink(job: JobView): Promise<void> {
  const fetchLink = async (): Promise<string> => {
    const { url } = await api.shareLink(job.id);
    return new URL(url, window.location.href).href;
  };
  const copied = () => showToast('Link copied — valid for 24 hours');
  try {
    // iOS Safari drops the tap's user-gesture activation across the fetch, so
    // the first choice hands the clipboard a promise it resolves later: the
    // write itself starts inside the gesture.
    if (typeof ClipboardItem !== 'undefined' && navigator.clipboard?.write) {
      await navigator.clipboard.write([
        new ClipboardItem({
          'text/plain': fetchLink().then((text) => new Blob([text], { type: 'text/plain' })),
        }),
      ]);
    } else if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(await fetchLink());
    } else {
      throw new Error('no clipboard API');
    }
    copied();
    return;
  } catch {
    // Older or insecure contexts: the deprecated execCommand path.
  }
  try {
    const scratch = document.createElement('textarea');
    scratch.value = await fetchLink();
    scratch.setAttribute('readonly', '');
    scratch.style.position = 'fixed';
    scratch.style.opacity = '0';
    document.body.appendChild(scratch);
    scratch.select();
    const ok = document.execCommand('copy');
    scratch.remove();
    if (!ok) throw new Error('copy failed');
    copied();
  } catch {
    showToast('Could not copy the link');
  }
}

async function retry(job: JobView): Promise<void> {
  try {
    await api.retry(job.id);
  } catch {
    showToast('Could not retry');
  }
  await refresh();
}

async function boot(): Promise<void> {
  const root = document.getElementById('app');
  if (!root) throw new Error('#app is missing');
  elements = initApp(root);
  list = createList(elements.list, {
    onDelete: confirmDelete,
    onCopy: (job) => void copyLink(job),
    onRetry: (job) => void retry(job),
  });
  elements.fab.addEventListener('click', showSubmit);

  try {
    const { csrf_token } = await api.session();
    setCsrfToken(csrf_token);
    startPolling();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      showLogin();
    } else {
      setOffline(elements, true);
      startPolling(); // keep trying; the banner stays until a poll succeeds
    }
  }
}

void boot();
