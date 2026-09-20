// Modal dialogs: login, URL submission, and delete confirmation. Each builder
// returns its element and a close function; behavior is wired through
// callbacks so main.ts stays free of DOM detail. Escape always cancels.

import { looksLikeYouTubeURL } from './api';
import { closeIcon } from './icons';

export interface ModalHandle {
  element: HTMLElement;
  close: () => void;
}

export type SubmitResult = 'ok' | 'duplicate' | { error: string };

function baseOverlay(kind: string, onCancel: () => void): HTMLElement {
  const overlay = document.createElement('div');
  overlay.className = `overlay overlay-${kind}`;
  overlay.addEventListener('modal-cancel', () => onCancel());
  overlay.addEventListener('keydown', (ev: KeyboardEvent) => {
    if (ev.key === 'Escape') {
      ev.preventDefault();
      overlay.dispatchEvent(new CustomEvent('modal-cancel', { bubbles: false }));
    }
  });
  return overlay;
}

function dialog(overlay: HTMLElement, title: string, closable: boolean): HTMLElement {
  const box = document.createElement('div');
  box.className = 'modal';
  box.setAttribute('role', 'dialog');
  box.setAttribute('aria-modal', 'true');
  box.setAttribute('aria-label', title);
  const heading = document.createElement('h2');
  heading.textContent = title;
  box.appendChild(heading);
  if (closable) {
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'modal-close';
    close.setAttribute('aria-label', 'Close dialog');
    close.innerHTML = closeIcon;
    close.addEventListener('click', () => overlay.dispatchEvent(new CustomEvent('modal-cancel', { bubbles: false })));
    box.appendChild(close);
  }
  overlay.appendChild(box);
  return box;
}

function mount(overlay: HTMLElement): ModalHandle {
  document.body.appendChild(overlay);
  overlay.querySelector<HTMLElement>('input, button')?.focus();
  return {
    element: overlay,
    close: () => overlay.remove(),
  };
}

function errorParagraph(): HTMLParagraphElement {
  const error = document.createElement('p');
  error.className = 'modal-error';
  error.setAttribute('role', 'alert');
  error.hidden = true;
  return error;
}

// onLogin resolves with null on success or an error message to display.
export function loginModal(onLogin: (password: string) => Promise<string | null>): ModalHandle {
  const overlay = baseOverlay('login', () => {}); // sign-in cannot be dismissed
  const box = dialog(overlay, 'Sign in', false);
  const form = document.createElement('form');
  const input = document.createElement('input');
  input.type = 'password';
  input.name = 'password';
  input.autocomplete = 'current-password';
  input.placeholder = 'Password';
  input.setAttribute('aria-label', 'Password');
  const error = errorParagraph();
  const submit = document.createElement('button');
  submit.type = 'submit';
  submit.className = 'primary';
  submit.textContent = 'Sign in';
  form.append(input, error, submit);
  box.appendChild(form);

  const handle = mount(overlay);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const message = await onLogin(input.value);
    if (message === null) {
      handle.close();
      return;
    }
    error.textContent = message;
    error.hidden = false;
  });
  return handle;
}

// onSubmit resolves 'ok' for a new job, 'duplicate' when the card already
// exists (the caller highlights it), or an error object to display.
export function submitModal(onSubmit: (url: string) => Promise<SubmitResult>): ModalHandle {
  const overlay = baseOverlay('submit', () => handle.close());
  const box = dialog(overlay, 'Add a video', true);
  const form = document.createElement('form');
  const input = document.createElement('input');
  input.type = 'url';
  input.name = 'url';
  input.inputMode = 'url';
  input.autocomplete = 'off';
  input.placeholder = 'Paste a YouTube link';
  input.setAttribute('aria-label', 'YouTube link');
  const error = errorParagraph();
  const submit = document.createElement('button');
  submit.type = 'submit';
  submit.className = 'primary';
  submit.textContent = 'Add';
  form.append(input, error, submit);
  box.appendChild(form);

  let busy = false;
  const handle = mount(overlay);

  async function attempt(raw: string): Promise<void> {
    if (busy) return;
    if (!looksLikeYouTubeURL(raw)) {
      error.textContent = 'That does not look like a YouTube link.';
      error.hidden = false;
      return;
    }
    busy = true;
    submit.disabled = true;
    const result = await onSubmit(raw.trim());
    busy = false;
    submit.disabled = false;
    if (result === 'ok') {
      handle.close();
      return;
    }
    if (result === 'duplicate') {
      // The caller highlights the existing card while we close.
      handle.close();
      return;
    }
    error.textContent = result.error;
    error.hidden = false;
  }

  // Submitting is explicit: paste only fills the field, the Add button (or
  // pressing Enter) runs the attempt.
  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    void attempt(input.value);
  });
  return handle;
}

// onConfirm resolves with null once deletion is done, or an error message.
export function confirmDeleteModal(title: string, onConfirm: () => Promise<string | null>): ModalHandle {
  const overlay = baseOverlay('confirm', () => handle.close());
  const box = dialog(overlay, 'Delete video', true);
  const name = document.createElement('p');
  name.className = 'modal-title-line';
  name.textContent = title === '' ? 'This video' : title;
  const warning = document.createElement('p');
  warning.className = 'modal-hint';
  warning.textContent = 'Deleting cancels any download and removes the stored file.';
  const error = errorParagraph();
  const row = document.createElement('div');
  row.className = 'modal-row';
  const cancel = document.createElement('button');
  cancel.type = 'button';
  cancel.textContent = 'Cancel';
  const confirm = document.createElement('button');
  confirm.type = 'button';
  confirm.className = 'danger';
  confirm.textContent = 'Delete';
  row.append(cancel, confirm);
  box.append(name, warning, error, row);

  const handle = mount(overlay);
  cancel.addEventListener('click', () => handle.close());
  confirm.addEventListener('click', async () => {
    confirm.disabled = true;
    cancel.disabled = true;
    const message = await onConfirm();
    if (message === null) {
      handle.close();
      return;
    }
    confirm.disabled = false;
    cancel.disabled = false;
    error.textContent = message;
    error.hidden = false;
  });
  return handle;
}
