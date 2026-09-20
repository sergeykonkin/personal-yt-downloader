// App chrome and card rendering. Cards are keyed by job id so the 2-second
// refresh patches existing elements instead of rebuilding the list — swipe
// state, focus, and transitions survive re-renders.

import { api, type JobView } from './api';
import { formatBytes } from './format';
import { downloadIcon, linkIcon, plusIcon, retryIcon } from './icons';
import { hasPercent, inFlight, percent, stageLabel } from './stages';
import { attachSwipe } from './swipe';

export interface CardHandlers {
  onDelete: (job: JobView) => void;
  onCopy: (job: JobView) => void;
  onRetry: (job: JobView) => void;
}

export interface AppElements {
  root: HTMLElement;
  list: HTMLElement;
  fab: HTMLButtonElement;
  footer: HTMLElement;
  total: HTMLElement;
  offline: HTMLElement;
}

// A card element carries its latest JobView so handlers act on fresh state.
interface JobCard extends HTMLElement {
  __job?: JobView;
}

function currentJob(card: JobCard): JobView {
  return card.__job ?? { id: card.dataset.id ?? '', title: '', status: '', stage: '', size_bytes: 0, created_at: '' };
}

export function initApp(root: HTMLElement): AppElements {
  root.innerHTML = '';

  const offline = document.createElement('div');
  offline.className = 'offline';
  offline.setAttribute('role', 'status');
  offline.textContent = 'Connection lost — retrying…';
  offline.hidden = true;

  const list = document.createElement('main');
  list.className = 'job-list';
  list.setAttribute('aria-label', 'Videos');

  const fab = document.createElement('button');
  fab.type = 'button';
  fab.className = 'fab';
  fab.setAttribute('aria-label', 'Add a video');
  fab.innerHTML = plusIcon;

  const footer = document.createElement('footer');
  footer.className = 'footer';
  const total = document.createElement('span');
  total.className = 'total';
  total.textContent = 'Total: 0 B';
  footer.appendChild(total);

  root.append(offline, list, fab, footer);
  return { root, list, fab, footer, total, offline };
}

interface CardRefs {
  title: HTMLElement;
  stage: HTMLElement;
  size: HTMLElement;
  bar: HTMLElement;
  fill: HTMLElement;
  error: HTMLElement;
  retry: HTMLElement;
  actions: HTMLElement;
}

const refs = new WeakMap<HTMLElement, CardRefs>();

export function buildCard(job: JobView, handlers: CardHandlers): HTMLElement {
  const card = document.createElement('article') as JobCard;
  card.className = 'card';
  card.dataset.id = job.id;
  card.setAttribute('aria-label', 'Video');

  // Red feedback behind the card while it is dragged; the swipe itself
  // opens the delete confirmation, so the underlay holds no button.
  const underlay = document.createElement('div');
  underlay.className = 'underlay';

  const surface = document.createElement('div');
  surface.className = 'surface';

  const title = document.createElement('h3');
  title.className = 'title';

  const meta = document.createElement('div');
  meta.className = 'meta';
  const stage = document.createElement('span');
  stage.className = 'stage';
  const size = document.createElement('span');
  size.className = 'size';
  meta.append(stage, size);

  const bar = document.createElement('div');
  bar.className = 'progress';
  const fill = document.createElement('div');
  fill.className = 'progress-fill';
  bar.appendChild(fill);

  const error = document.createElement('p');
  error.className = 'card-error';
  error.setAttribute('role', 'alert');
  const retry = document.createElement('button');
  retry.type = 'button';
  retry.className = 'chip retry-button';
  retry.innerHTML = `${retryIcon}<span>Retry</span>`;
  retry.setAttribute('aria-label', 'Retry download');

  const actions = document.createElement('div');
  actions.className = 'actions';
  const download = document.createElement('a');
  download.className = 'icon-button primary';
  download.setAttribute('aria-label', 'Download video');
  download.innerHTML = downloadIcon;
  const copy = document.createElement('button');
  copy.type = 'button';
  copy.className = 'icon-button';
  copy.setAttribute('aria-label', 'Copy video link');
  copy.innerHTML = linkIcon;
  actions.append(download, copy);

  surface.append(title, meta, bar, error, retry, actions);
  card.append(underlay, surface);

  refs.set(card, { title, stage, size, bar, fill, error, retry, actions });

  retry.addEventListener('click', () => handlers.onRetry(currentJob(card)));
  copy.addEventListener('click', () => handlers.onCopy(currentJob(card)));

  download.href = api.downloadURL(job.id);
  download.setAttribute('download', '');

  // Releasing a swipe past its halfway point asks for the delete
  // confirmation; the surface springs back on its own.
  attachSwipe({
    surface,
    onTrigger: () => handlers.onDelete(currentJob(card)),
  });

  updateCard(card, job);
  return card;
}

export function updateCard(card: HTMLElement, job: JobView): void {
  const r = refs.get(card);
  if (!r) return;
  (card as JobCard).__job = job;

  if (r.title.textContent !== job.title) r.title.textContent = job.title;

  const label = stageLabel(job);
  if (r.stage.textContent !== label) r.stage.textContent = label;

  const ready = job.status === 'ready';
  const failed = job.status === 'failed';
  r.size.textContent = ready ? formatBytes(job.size_bytes) : '';
  r.size.hidden = !ready;

  const showBar = inFlight(job);
  r.bar.hidden = !showBar;
  if (showBar) {
    if (hasPercent(job)) {
      r.bar.classList.remove('indeterminate');
      r.fill.style.width = `${percent(job)}%`;
    } else {
      r.bar.classList.add('indeterminate');
      r.fill.style.width = '';
    }
  }

  r.error.textContent = failed ? (job.error ?? '') : '';
  r.error.hidden = !failed;
  r.retry.hidden = !failed;
  r.actions.hidden = !ready;

  card.classList.toggle('is-ready', ready);
  card.classList.toggle('is-failed', failed);
}

export interface ListController {
  sync: (jobs: JobView[]) => void;
  cardFor: (id: string) => HTMLElement | undefined;
}

export function createList(list: HTMLElement, handlers: CardHandlers): ListController {
  const cards = new Map<string, HTMLElement>();
  let emptyHint: HTMLElement | null = null;

  return {
    sync(jobs: JobView[]) {
      const seen = new Set<string>();
      for (const job of jobs) {
        seen.add(job.id);
        let card = cards.get(job.id);
        if (!card) {
          card = buildCard(job, handlers);
          cards.set(job.id, card);
        } else {
          updateCard(card, job);
        }
        list.appendChild(card); // moves existing cards into list order
      }
      for (const [id, card] of cards) {
        if (!seen.has(id)) {
          card.remove();
          cards.delete(id);
        }
      }
      if (jobs.length === 0 && !emptyHint) {
        emptyHint = document.createElement('p');
        emptyHint.className = 'empty';
        emptyHint.textContent = 'No videos yet. Tap + to add one.';
        list.appendChild(emptyHint);
      } else if (jobs.length > 0 && emptyHint) {
        emptyHint.remove();
        emptyHint = null;
      }
    },
    cardFor: (id: string) => cards.get(id),
  };
}

export function setOffline(elements: AppElements, offline: boolean): void {
  elements.offline.hidden = !offline;
}

export function setTotal(elements: AppElements, bytes: number): void {
  elements.total.textContent = `Total: ${formatBytes(bytes)}`;
}

let toastTimer = 0;

export function showToast(message: string): void {
  let toast = document.querySelector<HTMLElement>('.toast');
  if (!toast) {
    toast = document.createElement('div');
    toast.className = 'toast';
    toast.setAttribute('role', 'status');
    document.body.appendChild(toast);
  }
  toast.textContent = message;
  toast.classList.add('show');
  window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => toast?.classList.remove('show'), 2600);
}

// highlightCard draws attention to an existing card after a duplicate submit.
export function highlightCard(card: HTMLElement): void {
  card.scrollIntoView?.({ behavior: 'smooth', block: 'center' });
  card.classList.remove('pulse');
  void card.offsetWidth; // restart a mid-flight animation
  card.classList.add('pulse');
  window.setTimeout(() => card.classList.remove('pulse'), 1600);
}
