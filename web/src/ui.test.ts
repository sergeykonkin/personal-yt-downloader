import { beforeEach, describe, expect, it, vi } from 'vitest';

import type { JobView } from './api';
import { createList, highlightCard, initApp, setOffline, setTotal, showToast, type AppElements } from './ui';

function job(partial: Partial<JobView> & { id: string }): JobView {
  return {
    title: 'Untitled',
    status: 'queued',
    stage: 'queued',
    size_bytes: 0,
    created_at: '2026-01-01T00:00:00Z',
    ...partial,
  };
}

let elements: AppElements;

beforeEach(() => {
  document.body.innerHTML = '<div id="app"></div>';
  elements = initApp(document.getElementById('app')!);
});

describe('card rendering', () => {
  it('shows titles, stage labels, sizes, and actions per status', () => {
    const list = createList(elements.list, {
      onDelete: () => {},
      onCopy: () => {},
      onRetry: () => {},
    });
    list.sync([
      job({ id: 'abcdefghijk', title: 'Cooking Pasta', status: 'ready', stage: 'ready', size_bytes: 45_600_000 }),
      job({ id: 'lmnopqrstuv', title: 'Lecture 12', status: 'downloading', stage: 'downloading.video', progress: 42 }),
      job({ id: 'wxyz0123456', title: 'Music Mix', status: 'processing', stage: 'processing' }),
      job({ id: 'qwertyuiopa', title: 'Broken', status: 'failed', stage: 'failed', error: 'Unable to download this video.' }),
    ]);

    const cards = [...elements.list.querySelectorAll('.card')];
    expect(cards).toHaveLength(4);

    const ready = cards[0];
    expect(ready.querySelector('.title')!.textContent).toBe('Cooking Pasta');
    expect(ready.querySelector('.stage')!.textContent).toBe('Ready');
    expect(ready.querySelector('.size')!.textContent).toBe('45.6 MB');
    expect(ready.querySelector<HTMLDivElement>('.actions')!.hidden).toBe(false);
    const download = ready.querySelector<HTMLAnchorElement>('a[aria-label="Download video"]')!;
    expect(download.getAttribute('href')).toBe('/api/jobs/abcdefghijk/download');
    expect(download.hasAttribute('download')).toBe(true);

    const downloading = cards[1];
    expect(downloading.querySelector('.stage')!.textContent).toBe('Downloading video');
    const bar = downloading.querySelector<HTMLElement>('.progress')!;
    expect(bar.hidden).toBe(false);
    expect(bar.classList.contains('indeterminate')).toBe(false);
    expect(downloading.querySelector<HTMLElement>('.progress-fill')!.style.width).toBe('42%');

    const processing = cards[2];
    expect(processing.querySelector('.progress')!.classList.contains('indeterminate')).toBe(true);

    const failed = cards[3];
    expect(failed.querySelector('.card-error')!.textContent).toContain('Unable to download');
    expect(failed.querySelector<HTMLElement>('.retry-button')!.hidden).toBe(false);
    expect(failed.querySelector<HTMLDivElement>('.actions')!.hidden).toBe(true);
  });

  it('patches existing cards and removes deleted ones', () => {
    const onDelete = vi.fn();
    const list = createList(elements.list, { onDelete, onCopy: () => {}, onRetry: () => {} });
    list.sync([job({ id: 'abcdefghijk', title: 'First Title' })]);
    const card = elements.list.querySelector('.card')!;

    list.sync([job({ id: 'abcdefghijk', title: 'New Title', status: 'downloading', stage: 'downloading.video', progress: 7 })]);
    expect(card.querySelector('.title')!.textContent).toBe('New Title');
    expect(card.querySelector<HTMLElement>('.progress-fill')!.style.width).toBe('7%');

    // Handlers receive the latest job state, not the state at build time.
    // The swipe is the only delete path: past halfway it fires onDelete.
    expect(card.querySelector('.delete-button')).toBeNull();
    expect(card.querySelector('.underlay')!.childElementCount).toBe(0);
    const surface = card.querySelector<HTMLElement>('.surface')!;
    const swipe = (type: string, x: number) => {
      const ev = new Event(type, { bubbles: true, cancelable: true });
      const touch = { identifier: 1, clientX: x, clientY: 300 };
      Object.defineProperty(ev, 'touches', { value: { length: 1, 0: touch } });
      Object.defineProperty(ev, 'changedTouches', { value: { length: 1, 0: touch } });
      surface.dispatchEvent(ev);
    };
    swipe('touchstart', 200);
    swipe('touchmove', 100);
    swipe('touchend', 100);
    expect(onDelete).toHaveBeenCalledTimes(1);
    const passed = onDelete.mock.calls[0][0] as JobView;
    expect(passed.title).toBe('New Title');

    list.sync([]);
    expect(elements.list.querySelectorAll('.card')).toHaveLength(0);
    expect(elements.list.querySelector('.empty')!.textContent).toContain('No videos yet');
  });
});

describe('footer, banner, and toast', () => {
  it('formats the footer total', () => {
    setTotal(elements, 8_400_000_000);
    expect(elements.total.textContent).toBe('Total: 8.4 GB');
  });

  it('toggles the offline banner for connection recovery', () => {
    setOffline(elements, true);
    expect(elements.offline.hidden).toBe(false);
    setOffline(elements, false);
    expect(elements.offline.hidden).toBe(true);
  });

  it('shows a transient toast', () => {
    vi.useFakeTimers();
    showToast('Link copied');
    const toast = document.querySelector<HTMLElement>('.toast')!;
    expect(toast.textContent).toBe('Link copied');
    expect(toast.classList.contains('show')).toBe(true);
    vi.advanceTimersByTime(3000);
    expect(toast.classList.contains('show')).toBe(false);
    vi.useRealTimers();
  });
});

describe('duplicate highlight and keyboard access', () => {
  it('pulses an existing card', () => {
    vi.useFakeTimers();
    const list = createList(elements.list, { onDelete: () => {}, onCopy: () => {}, onRetry: () => {} });
    list.sync([job({ id: 'abcdefghijk', title: 'Again' })]);
    const card = list.cardFor('abcdefghijk')!;

    highlightCard(card);
    expect(card.classList.contains('pulse')).toBe(true);
    vi.advanceTimersByTime(2000);
    expect(card.classList.contains('pulse')).toBe(false);
    vi.useRealTimers();
  });

  it('exposes accessible, focusable controls', () => {
    const list = createList(elements.list, { onDelete: () => {}, onCopy: () => {}, onRetry: () => {} });
    list.sync([job({ id: 'abcdefghijk', status: 'ready', stage: 'ready', size_bytes: 10 })]);
    const card = list.cardFor('abcdefghijk')!;

    for (const label of ['Download video', 'Copy video link']) {
      const el = card.querySelector(`[aria-label="${label}"]`)!;
      expect(el).not.toBeNull();
      expect(el.hasAttribute('disabled')).toBe(false);
      expect(el.tagName).toMatch(/BUTTON|A/);
    }
    // The FAB is reachable by keyboard.
    expect(elements.fab.getAttribute('aria-label')).toBe('Add a video');
    expect(elements.fab.tagName).toBe('BUTTON');
  });
});
