import { describe, expect, it, vi } from 'vitest';

import type { JobView } from './api';
import { confirmDeleteModal, loginModal, submitModal } from './modals';

function submitForm(overlay: HTMLElement): void {
  const form = overlay.querySelector('form')!;
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

describe('login modal', () => {
  it('shows the error message and stays open on failure', async () => {
    const onLogin = vi.fn().mockResolvedValue('Incorrect password.');
    const handle = loginModal(onLogin);

    const input = handle.element.querySelector<HTMLInputElement>('input')!;
    input.value = 'wrong';
    submitForm(handle.element);

    await vi.waitFor(() => {
      expect(onLogin).toHaveBeenCalledWith('wrong');
    });
    const error = handle.element.querySelector('.modal-error') as HTMLElement;
    expect(error.hidden).toBe(false);
    expect(error.textContent).toBe('Incorrect password.');
    expect(document.body.contains(handle.element)).toBe(true);
    handle.close();
  });

  it('closes on success', async () => {
    const handle = loginModal(vi.fn().mockResolvedValue(null));
    submitForm(handle.element);
    await vi.waitFor(() => {
      expect(document.body.contains(handle.element)).toBe(false);
    });
  });
});

describe('submit modal', () => {
  it('does not auto-submit pasted links — the Add button submits', async () => {
    const onSubmit = vi.fn().mockResolvedValue('ok');
    const handle = submitModal(onSubmit);
    const input = handle.element.querySelector<HTMLInputElement>('input')!;

    input.value = 'https://www.youtube.com/watch?v=dQw4w9WgXcQ';
    input.dispatchEvent(new Event('paste', { bubbles: true }));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(onSubmit).not.toHaveBeenCalled(); // paste only fills the field

    submitForm(handle.element);
    await vi.waitFor(() => {
      expect(onSubmit).toHaveBeenCalledWith('https://www.youtube.com/watch?v=dQw4w9WgXcQ');
    });
    await vi.waitFor(() => {
      expect(document.body.contains(handle.element)).toBe(false);
    });
  });

  it('shows an in-modal error for invalid manual entry', async () => {
    const onSubmit = vi.fn();
    const handle = submitModal(onSubmit);
    const input = handle.element.querySelector<HTMLInputElement>('input')!;
    input.value = 'not a link';
    submitForm(handle.element);

    const error = handle.element.querySelector('.modal-error') as HTMLElement;
    await vi.waitFor(() => {
      expect(error.hidden).toBe(false);
    });
    expect(error.textContent).toContain('YouTube');
    expect(onSubmit).not.toHaveBeenCalled();
    handle.close();
  });

  it('displays server errors and keeps the modal open', async () => {
    const handle = submitModal(vi.fn().mockResolvedValue({ error: 'The queue is full.' }));
    const input = handle.element.querySelector<HTMLInputElement>('input')!;
    input.value = 'https://youtu.be/dQw4w9WgXcQ';
    submitForm(handle.element);
    const error = handle.element.querySelector('.modal-error') as HTMLElement;
    await vi.waitFor(() => {
      expect(error.textContent).toBe('The queue is full.');
    });
    expect(document.body.contains(handle.element)).toBe(true);
    handle.close();
  });

  it('closes on Escape', async () => {
    const handle = submitModal(vi.fn());
    const input = handle.element.querySelector<HTMLInputElement>('input')!;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(document.body.contains(handle.element)).toBe(false);
  });
});

describe('confirm delete modal', () => {
  const job: JobView = {
    id: 'abcdefghijk',
    title: 'A Title',
    status: 'ready',
    stage: 'ready',
    size_bytes: 1000,
    created_at: '2026-01-01T00:00:00Z',
  };

  it('cancel closes without deleting', async () => {
    const onConfirm = vi.fn();
    const handle = confirmDeleteModal(job.title, onConfirm);

    expect(handle.element.textContent).toContain('A Title');

    const cancel = [...handle.element.querySelectorAll('button')].find((b) => b.textContent === 'Cancel')!;
    cancel.click();
    expect(onConfirm).not.toHaveBeenCalled();
    expect(document.body.contains(handle.element)).toBe(false);
  });

  it('confirm deletes and closes after the server responds', async () => {
    const onConfirm = vi.fn().mockResolvedValue(null);
    const handle = confirmDeleteModal(job.title, onConfirm);

    const confirm = [...handle.element.querySelectorAll('button')].find((b) => b.textContent === 'Delete')!;
    confirm.click();
    expect(document.body.contains(handle.element)).toBe(true); // waits
    await vi.waitFor(() => {
      expect(document.body.contains(handle.element)).toBe(false);
    });
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it('shows an error and stays open when deletion fails', async () => {
    const handle = confirmDeleteModal(job.title, vi.fn().mockResolvedValue('Storage error.'));
    const confirm = [...handle.element.querySelectorAll('button')].find((b) => b.textContent === 'Delete')!;
    confirm.click();
    const error = handle.element.querySelector('.modal-error') as HTMLElement;
    await vi.waitFor(() => {
      expect(error.hidden).toBe(false);
    });
    expect(error.textContent).toBe('Storage error.');
    handle.close();
  });

  it('Escape counts as cancellation', () => {
    const onConfirm = vi.fn();
    const handle = confirmDeleteModal(job.title, onConfirm);
    handle.element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: false }));
    expect(document.body.contains(handle.element)).toBe(false);
    expect(onConfirm).not.toHaveBeenCalled();
  });
});
