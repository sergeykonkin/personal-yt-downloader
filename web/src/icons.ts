// Inline stroke icons (24×24, currentColor) so the bundle needs no requests.

function svg(body: string): string {
  return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${body}</svg>`;
}

export const plusIcon = svg('<path d="M12 5v14M5 12h14" />');

export const downloadIcon = svg('<path d="M12 4v11" /><path d="m7 11 5 5 5-5" /><path d="M5 20h14" />');

export const linkIcon = svg(
  '<path d="M10 13a5 5 0 0 0 7.07 0l2.12-2.12a5 5 0 0 0-7.07-7.07L10.7 5.3" />' +
    '<path d="M14 11a5 5 0 0 0-7.07 0L4.8 13.12a5 5 0 0 0 7.07 7.07L13.3 18.7" />',
);

export const closeIcon = svg('<path d="M6 6l12 12M18 6L6 18" />');

export const retryIcon = svg(
  '<path d="M21 12a9 9 0 1 1-2.64-6.36" /><path d="M21 3v6h-6" />',
);
