import { afterEach, describe, expect, it, vi } from 'vitest';

import { attachSwipe } from './swipe';

// jsdom has no TouchEvent constructor; the handlers only read touches,
// changedTouches, and the per-touch coordinates, so plain Events carrying a
// fake touch list drive them exactly like real touches.
interface FakeTouch {
  identifier: number;
  clientX: number;
  clientY: number;
}

function touchEvent(type: string, t: FakeTouch): Event {
  const ev = new Event(type, { bubbles: true, cancelable: true });
  const list = { length: 1, 0: t };
  Object.defineProperty(ev, 'touches', { value: list });
  Object.defineProperty(ev, 'changedTouches', { value: list });
  return ev;
}

function touch(target: HTMLElement | Window, type: string, t: FakeTouch): Event {
  const ev = touchEvent(type, t);
  target.dispatchEvent(ev);
  return ev;
}

function mouse(target: HTMLElement | Window, type: string, x: number, y: number): MouseEvent {
  const ev = new MouseEvent(type, { bubbles: true, cancelable: true, clientX: x, clientY: y });
  target.dispatchEvent(ev);
  return ev;
}

function surface(): HTMLElement {
  const el = document.createElement('div');
  document.body.appendChild(el);
  return el;
}

let detachAll: Array<() => void> = [];
afterEach(() => {
  for (const detach of detachAll) detach();
  detachAll = [];
  document.body.innerHTML = '';
});

describe('swipe', () => {
  it('translates while dragging, then springs back and triggers on release', () => {
    const el = surface();
    const onTrigger = vi.fn();
    detachAll.push(attachSwipe({ surface: el, onTrigger }));

    touch(el, 'touchstart', { identifier: 7, clientX: 200, clientY: 300 });
    const move = touch(el, 'touchmove', { identifier: 7, clientX: 150, clientY: 302 });
    expect(move.defaultPrevented).toBe(true); // horizontal drags are ours
    expect(el.style.transform).toBe('translateX(-50px)');

    touch(el, 'touchend', { identifier: 7, clientX: 150, clientY: 302 });
    expect(el.style.transform).toBe('');
    expect(el.classList.contains('settling')).toBe(true);
    expect(onTrigger).toHaveBeenCalledTimes(1);
  });

  it('supports mouse drags through window listeners', () => {
    const el = surface();
    const onTrigger = vi.fn();
    detachAll.push(attachSwipe({ surface: el, onTrigger }));

    mouse(el, 'mousedown', 200, 300);
    const move = mouse(window, 'mousemove', 120, 300);
    expect(move.defaultPrevented).toBe(true);
    expect(el.style.transform).toBe('translateX(-80px)');
    mouse(window, 'mouseup', 120, 300);
    expect(el.style.transform).toBe('');
    expect(el.classList.contains('settling')).toBe(true);
    expect(onTrigger).toHaveBeenCalledTimes(1);
  });

  it('recovers a swipe whose opening samples drift vertically', () => {
    const el = surface();
    const onTrigger = vi.fn();
    detachAll.push(attachSwipe({ surface: el, onTrigger }));

    touch(el, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    // First samples drift down-left: diagonal, not yet decidable — and it
    // must not kill the gesture.
    let move = touch(el, 'touchmove', { identifier: 1, clientX: 194, clientY: 312 });
    expect(move.defaultPrevented).toBe(false);
    expect(el.style.transform).toBe('');
    // The swipe turns horizontal and takes over.
    move = touch(el, 'touchmove', { identifier: 1, clientX: 120, clientY: 316 });
    expect(move.defaultPrevented).toBe(true);
    expect(el.style.transform).toBe('translateX(-80px)');
    touch(el, 'touchend', { identifier: 1, clientX: 120, clientY: 316 });
    expect(el.style.transform).toBe('');
    expect(onTrigger).toHaveBeenCalledTimes(1);
  });

  it('never prevents vertical movement', () => {
    const el = surface();
    detachAll.push(attachSwipe({ surface: el, onTrigger: () => {} }));

    touch(el, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    const move = touch(el, 'touchmove', { identifier: 1, clientX: 198, clientY: 380 });
    expect(move.defaultPrevented).toBe(false);
    expect(el.style.transform).toBe('');

    const up = touch(el, 'touchend', { identifier: 1, clientX: 198, clientY: 380 });
    expect(up.defaultPrevented).toBe(false);
  });

  it('ignores movement below the lock threshold', () => {
    const el = surface();
    detachAll.push(attachSwipe({ surface: el, onTrigger: () => {} }));
    touch(el, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    const move = touch(el, 'touchmove', { identifier: 1, clientX: 197, clientY: 299 });
    expect(move.defaultPrevented).toBe(false);
    expect(el.style.transform).toBe('');
  });

  it('clamps rightward movement and does not trigger on a short drag', () => {
    const el = surface();
    const onTrigger = vi.fn();
    detachAll.push(attachSwipe({ surface: el, onTrigger }));

    touch(el, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    touch(el, 'touchmove', { identifier: 1, clientX: 400, clientY: 300 }); // rightward
    expect(el.style.transform).toBe('');
    touch(el, 'touchend', { identifier: 1, clientX: 400, clientY: 300 });
    expect(onTrigger).not.toHaveBeenCalled();

    touch(el, 'touchstart', { identifier: 2, clientX: 200, clientY: 300 });
    touch(el, 'touchmove', { identifier: 2, clientX: 170, clientY: 300 }); // -30px
    touch(el, 'touchend', { identifier: 2, clientX: 170, clientY: 300 });
    expect(el.style.transform).toBe('');
    expect(el.classList.contains('settling')).toBe(true);
    expect(onTrigger).not.toHaveBeenCalled();
  });

  it('settles back without triggering when the gesture is cancelled', () => {
    const el = surface();
    const onTrigger = vi.fn();
    detachAll.push(attachSwipe({ surface: el, onTrigger }));

    touch(el, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    touch(el, 'touchmove', { identifier: 1, clientX: 140, clientY: 300 });
    touch(el, 'touchcancel', { identifier: 1, clientX: 140, clientY: 300 });
    expect(el.style.transform).toBe('');
    expect(el.classList.contains('settling')).toBe(true);
    expect(onTrigger).not.toHaveBeenCalled();
  });

  it('does not swipe from buttons', () => {
    const el = surface();
    const button = document.createElement('button');
    button.textContent = 'x';
    el.appendChild(button);
    detachAll.push(attachSwipe({ surface: el, onTrigger: () => {} }));

    touch(button, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    touch(el, 'touchmove', { identifier: 1, clientX: 100, clientY: 300 });
    expect(el.style.transform).toBe('');
  });

  it('ignores the click a finished swipe can emit', () => {
    const el = surface();
    const onTrigger = vi.fn();
    detachAll.push(attachSwipe({ surface: el, onTrigger }));

    touch(el, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    touch(el, 'touchmove', { identifier: 1, clientX: 100, clientY: 300 });
    touch(el, 'touchend', { identifier: 1, clientX: 100, clientY: 300 });
    mouse(el, 'click', 150, 300);
    expect(onTrigger).toHaveBeenCalledTimes(1);
  });

  it('tracks a new drag directly after a settle', () => {
    const el = surface();
    const onTrigger = vi.fn();
    detachAll.push(attachSwipe({ surface: el, onTrigger }));

    touch(el, 'touchstart', { identifier: 1, clientX: 200, clientY: 300 });
    touch(el, 'touchmove', { identifier: 1, clientX: 100, clientY: 300 });
    touch(el, 'touchend', { identifier: 1, clientX: 100, clientY: 300 });
    expect(el.classList.contains('settling')).toBe(true);

    // The settle class must not smooth the next drag: it tracks 1:1.
    touch(el, 'touchstart', { identifier: 2, clientX: 200, clientY: 300 });
    expect(el.classList.contains('settling')).toBe(false);
    const move = touch(el, 'touchmove', { identifier: 2, clientX: 160, clientY: 300 });
    expect(move.defaultPrevented).toBe(true);
    expect(el.style.transform).toBe('translateX(-40px)');
  });
});
