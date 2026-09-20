import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

// The stylesheet is loaded as text so the layout invariants — iPhone safe
// areas, dark-mode support, and scroll-preserving swipe behavior — are locked
// by tests even though jsdom cannot render CSS. (Vitest stubs CSS imports,
// so the file is read directly; vitest's cwd is the project root.)
const css = readFileSync(path.resolve(process.cwd(), 'src', 'style.css'), 'utf8');

describe('safe areas and appearance', () => {
  it('respects the bottom safe area for the footer, FAB, and list', () => {
    expect(css).toContain('env(safe-area-inset-bottom');
    expect(css).toContain('env(safe-area-inset-top');
    expect(css).toContain('env(safe-area-inset-right');
  });

  it('follows the device color scheme', () => {
    expect(css).toContain('@media (prefers-color-scheme: dark)');
    expect(css).toContain('color-scheme: light dark');
  });

  it('lets vertical swipes scroll while claiming horizontal ones', () => {
    expect(css).toContain('touch-action: pan-y');
  });

  it('springs a released swipe back to rest', () => {
    expect(css).toContain('.surface.settling');
    expect(css).toContain('transition: transform');
  });

  it('keeps an indeterminate progress animation', () => {
    expect(css).toContain('.progress.indeterminate');
  });

  it('honors reduced-motion preferences', () => {
    expect(css).toContain('@media (prefers-reduced-motion: reduce)');
  });
});
