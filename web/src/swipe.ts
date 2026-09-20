// Swipe-right-to-left opens the delete confirmation: the surface tracks the
// finger while dragging, and releasing past the halfway point fires
// onTrigger while the surface springs back to rest on its own. Pointer
// events proved unreliable on iOS Safari, so touches and mouse drags are
// tracked directly. Vertical movement is never claimed, so the page keeps
// scrolling; the surface carries `touch-action: pan-y` for the browser too.

export interface SwipeTarget {
  surface: HTMLElement;
  onTrigger: () => void;
}

const LOCK_THRESHOLD = 8; // px of movement before a direction is chosen
const DRAG_WIDTH = 88; // px of leftward drag available to the gesture
const TRIGGER_DRAG = DRAG_WIDTH / 2; // releasing beyond this triggers

export function attachSwipe({ surface, onTrigger }: SwipeTarget): () => void {
  let tracking = false; // a gesture that may still become a swipe
  let horizontal = false;
  let touchID: number | null = null;
  let startX = 0;
  let startY = 0;
  let offset = 0; // current translate, 0..-DRAG_WIDTH
  let base = 0; // translate at gesture start

  // begin returns false when an interactive element owns the gesture.
  function begin(x: number, y: number, target: EventTarget | null): boolean {
    if ((target as HTMLElement | null)?.closest('button, a, input, textarea')) return false;
    tracking = true;
    horizontal = false;
    startX = x;
    startY = y;
    base = offset;
    surface.classList.remove('settling'); // a new drag tracks the finger 1:1
    return true;
  }

  // move reports whether the caller should preventDefault. The first
  // decisive sample picks the axis: horizontal once |dx| leads, vertical
  // (and dead to us) only once |dy| is twice |dx| — a swipe whose opening
  // samples drift downward still recovers when it turns horizontal.
  function move(x: number, y: number): boolean {
    if (!tracking) return false;
    const dx = x - startX;
    const dy = y - startY;
    if (!horizontal) {
      const ax = Math.abs(dx);
      const ay = Math.abs(dy);
      if (ax <= LOCK_THRESHOLD && ay <= LOCK_THRESHOLD) return false;
      if (ax > LOCK_THRESHOLD && ax >= ay) {
        horizontal = true;
      } else if (ay > LOCK_THRESHOLD && ay > 2 * ax) {
        tracking = false; // a vertical scroll; leave it to the browser
        return false;
      } else {
        return false; // diagonal so far; wait for one axis to dominate
      }
    }
    offset = clamp(base + Math.min(0, dx));
    surface.style.transform = offset === 0 ? '' : `translateX(${offset}px)`;
    return true;
  }

  // finish springs the surface back to rest; a completed swipe past the
  // halfway point fires the trigger. A cancelled gesture only settles.
  function finish(finished: boolean): void {
    if (!tracking) return;
    tracking = false;
    if (!horizontal) return;
    horizontal = false;
    const triggered = finished && offset < -TRIGGER_DRAG;
    offset = 0;
    surface.classList.add('settling');
    surface.style.transform = '';
    if (triggered) onTrigger();
  }

  function findTouch(list: TouchList): Touch | null {
    for (let i = 0; i < list.length; i++) {
      const t = list[i];
      if (t.identifier === touchID) return t;
    }
    return null;
  }

  function onTouchStart(ev: TouchEvent) {
    if (tracking || ev.touches.length !== 1) return;
    const t = ev.touches[0];
    if (!t || !begin(t.clientX, t.clientY, ev.target)) return;
    touchID = t.identifier;
  }

  function onTouchMove(ev: TouchEvent) {
    if (!tracking) return;
    const t = findTouch(ev.changedTouches);
    if (t && move(t.clientX, t.clientY)) ev.preventDefault();
  }

  function onTouchEnd(ev: TouchEvent) {
    if (!tracking || !findTouch(ev.changedTouches)) return;
    touchID = null;
    finish(ev.type === 'touchend');
  }

  function onMouseDown(ev: MouseEvent) {
    begin(ev.clientX, ev.clientY, ev.target);
  }

  function onMouseMove(ev: MouseEvent) {
    if (move(ev.clientX, ev.clientY)) ev.preventDefault();
  }

  function onMouseUp() {
    finish(true);
  }

  surface.addEventListener('touchstart', onTouchStart, { passive: true });
  surface.addEventListener('touchmove', onTouchMove, { passive: false });
  surface.addEventListener('touchend', onTouchEnd);
  surface.addEventListener('touchcancel', onTouchEnd);
  surface.addEventListener('mousedown', onMouseDown);
  window.addEventListener('mousemove', onMouseMove);
  window.addEventListener('mouseup', onMouseUp);

  return function detach() {
    surface.removeEventListener('touchstart', onTouchStart);
    surface.removeEventListener('touchmove', onTouchMove);
    surface.removeEventListener('touchend', onTouchEnd);
    surface.removeEventListener('touchcancel', onTouchEnd);
    surface.removeEventListener('mousedown', onMouseDown);
    window.removeEventListener('mousemove', onMouseMove);
    window.removeEventListener('mouseup', onMouseUp);
  };
}

function clamp(value: number): number {
  return Math.max(-DRAG_WIDTH, Math.min(0, value));
}
