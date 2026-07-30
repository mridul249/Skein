import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { createPortal } from 'react-dom';
import clsx from 'clsx';

/**
 * The one overlay primitive: a panel anchored to a trigger, rendered in a
 * portal so no ancestor's `overflow` can touch it.
 *
 * It replaces two separate hover panels that were each broken in their own
 * way, and it exists as a primitive rather than a fix in two places because
 * the quota bar's tooltip survives Session 4c even though the shard popover's
 * call site does not.
 *
 * Three defects it is built to make impossible (known issue #27):
 *
 * 1. **Clipping.** An absolutely positioned panel is clipped by any ancestor
 *    with `overflow` other than visible whose box its containing-block chain
 *    passes through. `overflow-x: auto` computes `overflow-y: auto` as well,
 *    so the file table's card clipped 171px off the bottom of the shard map —
 *    the signature element of the whole product — and had done since it was
 *    built. A portal to `document.body` plus `position: fixed` removes the
 *    entire class: there is no ancestor left to clip against.
 *
 * 2. **The trigger moving out from under the pointer.** The old panel was a
 *    DOM child of the scrolling card, so opening it pushed the card's
 *    scrollHeight past its clientHeight, a vertical scrollbar materialised,
 *    the content box narrowed by 10px and the trigger jumped 10px left —
 *    out from under the cursor that had just opened it. Measured:
 *    clientWidth 974 → 964, trigger x 946 → 936. A portalled panel adds
 *    nothing to any ancestor's scroll height, so the trigger cannot move.
 *
 * 3. **The dead zone.** The panel sat 8px below the trigger with nothing
 *    hoverable in between, and `mouseleave` on the trigger closed it
 *    immediately — so the panel was not merely clipped, it was *unreachable*.
 *    Fixed by removing the gap rather than by timing it out: the portal root
 *    is a transparent wrapper whose padding spans the visual gap and whose
 *    edge touches the trigger, so trigger and panel hover areas are
 *    contiguous and there is no dead zone to cross. The close delay below is
 *    a second line of defence, not the mechanism.
 */

type Placement = 'top' | 'bottom';

/**
 * Grace period after the pointer leaves both trigger and panel.
 *
 * This is deliberately *not* what makes the panel reachable — the contiguous
 * hover area does that. It only covers a pointer that clips a corner on its
 * way in. Relying on a timeout instead was measured to fail: crossing an 8px
 * dead zone slowly takes longer than any delay short enough not to read as
 * lag, so the geometry has to be right and the delay is the cushion.
 */
const CLOSE_DELAY_MS = 120;

/** Distance between trigger and panel. */
const GAP = 8;

/** Minimum breathing room between the panel and the viewport edge. */
const MARGIN = 8;

export function Overlay({
  label,
  content,
  children,
  className,
  panelClassName,
  placement = 'top',
  disabled = false,
}: {
  /** Accessible name for the trigger, which is also the described element. */
  label: string;
  /** Panel body. This is content, not decoration — it carries real numbers. */
  content: ReactNode;
  /** Visible trigger content. Omit for a trigger that is purely a coloured box. */
  children?: ReactNode;
  /** Visual classes for the trigger. */
  className?: string;
  /** Extra classes for the panel, e.g. a width. */
  panelClassName?: string;
  placement?: Placement;
  /** Renders an inert styled box instead. For the compact sidebar rail. */
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ top: number; left: number; below: boolean } | null>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const closeTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const id = useId();

  const openNow = useCallback(() => {
    clearTimeout(closeTimer.current);
    setOpen(true);
  }, []);

  // Trigger and panel share this one state, so travelling between them is a
  // cancel rather than a close.
  const closeSoon = useCallback(() => {
    clearTimeout(closeTimer.current);
    closeTimer.current = setTimeout(() => setOpen(false), CLOSE_DELAY_MS);
  }, []);

  const closeNow = useCallback(() => {
    clearTimeout(closeTimer.current);
    setOpen(false);
  }, []);

  useEffect(() => () => clearTimeout(closeTimer.current), []);

  // Position from the trigger's viewport rect, flipping and clamping so the
  // panel is never cut off by an edge. Layout effect: measured and placed
  // before paint, so it never appears in the wrong place first.
  useLayoutEffect(() => {
    if (!open) {
      setPos(null);
      return;
    }
    const place = () => {
      const t = triggerRef.current?.getBoundingClientRect();
      const p = panelRef.current?.getBoundingClientRect();
      if (!t || !p) return;

      const vw = document.documentElement.clientWidth;
      const vh = document.documentElement.clientHeight;

      // Does the visual panel plus its gap fit on each side?
      const fitsBelow = t.bottom + GAP + p.height + MARGIN <= vh;
      const fitsAbove = t.top - GAP - p.height - MARGIN >= 0;
      const below = placement === 'bottom' ? fitsBelow || !fitsAbove : !fitsAbove && fitsBelow;

      // The wrapper's edge sits flush against the trigger and its padding
      // spans the gap, so the two hover areas touch. `top` is the wrapper's
      // top, which is why the above case subtracts the gap as well.
      let top = below ? t.bottom : t.top - p.height - GAP;
      const wrapperHeight = p.height + GAP;
      top = Math.min(Math.max(MARGIN, top), Math.max(MARGIN, vh - wrapperHeight - MARGIN));

      let left = t.left + t.width / 2 - p.width / 2;
      left = Math.min(Math.max(MARGIN, left), Math.max(MARGIN, vw - p.width - MARGIN));

      setPos({ top, left, below });
    };

    place();
    // `true` for capture: an ancestor scrolling must move the panel too, and
    // scroll events from a nested scroller do not bubble.
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    return () => {
      window.removeEventListener('scroll', place, true);
      window.removeEventListener('resize', place);
    };
  }, [open, placement]);

  // Esc closes from anywhere while open, matching the modal's standard.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        closeNow();
        triggerRef.current?.focus();
      }
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
  }, [open, closeNow]);

  if (disabled) {
    // Inert on purpose: the sidebar rail's per-drive rows state these numbers
    // as text directly beneath, so a second keyboard stop per drive would be
    // noise rather than access. The parent marks the whole rail aria-hidden.
    return <div className={className} title={label} />;
  }

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        aria-label={label}
        aria-describedby={open ? id : undefined}
        aria-expanded={open}
        className={className}
        onMouseEnter={openNow}
        onMouseLeave={closeSoon}
        onFocus={openNow}
        onBlur={closeSoon}
        onClick={() => (open ? closeNow() : openNow())}
      >
        {children}
      </button>

      {open &&
        createPortal(
          <div
            data-overlay-root
            onMouseEnter={openNow}
            onMouseLeave={closeSoon}
            className="fixed z-50 w-max"
            style={{
              top: pos ? `${pos.top}px` : 0,
              left: pos ? `${pos.left}px` : 0,
              // Transparent padding spanning the visual gap. This is the
              // bridge: with it, trigger and panel hover areas are one
              // continuous region and the pointer never crosses dead space.
              paddingTop: pos?.below === false ? 0 : GAP,
              paddingBottom: pos?.below === false ? GAP : 0,
              // Measured before it is shown, so it never flashes at 0,0.
              visibility: pos ? 'visible' : 'hidden',
            }}
          >
            <div
              ref={panelRef}
              id={id}
              role="tooltip"
              className={clsx(
                'w-max max-w-[min(90vw,32rem)] border border-surface0 bg-base',
                'px-1ch py-halfline text-left shadow-none',
                panelClassName,
              )}
            >
              {content}
            </div>
          </div>,
          document.body,
        )}
    </>
  );
}
