import { useId, useState, type ReactNode } from 'react';
import clsx from 'clsx';

/**
 * A small tooltip that is reachable by keyboard, not only by hover.
 *
 * Hover-only tooltips hide information from anyone navigating with a keyboard
 * or a screen reader, and the quota bar's tooltip carries the actual numbers —
 * it is the content, not decoration. So the trigger is focusable, opens on
 * focus as well as hover, closes on Escape, and is wired to the panel with
 * aria-describedby.
 *
 * **The trigger and the described element are the same node, deliberately.**
 * An earlier version put `aria-describedby` on a wrapper and `tabIndex` on a
 * child inside it. That renders identically and is silently useless: a screen
 * reader announces the description of the element that *has focus*, and focus
 * was landing on the child, which carried no description. The two attributes
 * have to meet on one element, so this component styles its own trigger rather
 * than wrapping one supplied by the caller.
 */
export function Tooltip({
  content,
  label,
  className,
  disabled = false,
}: {
  /** The panel body. Carries the real numbers. */
  content: ReactNode;
  /** Accessible name for the trigger, which is the described element. */
  label: string;
  /** Visual classes for the trigger itself. */
  className?: string;
  /** Skips the tooltip entirely, for the compact sidebar rail. */
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const id = useId();

  if (disabled) {
    // Not focusable and not described: the rail's per-drive rows directly
    // below already carry these numbers as real text, so a second keyboard
    // stop per drive would be noise rather than access. `title` keeps it
    // discoverable by pointer.
    return <div className={className} role="img" aria-label={label} title={label} />;
  }

  return (
    <div className="relative h-full w-full">
      <div
        tabIndex={0}
        role="img"
        aria-label={label}
        aria-describedby={open ? id : undefined}
        className={className}
        onMouseEnter={() => setOpen(true)}
        onMouseLeave={() => setOpen(false)}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onKeyDown={(e) => {
          if (e.key === 'Escape' && open) {
            e.stopPropagation();
            setOpen(false);
          }
        }}
      />

      {open && (
        <div
          id={id}
          role="tooltip"
          className={clsx(
            'pointer-events-none absolute bottom-full left-1/2 z-30 mb-2 w-max max-w-xs',
            '-translate-x-1/2 border border-surface0 bg-base px-1ch py-halfline',
            'text-left shadow-none',
          )}
        >
          {content}
        </div>
      )}
    </div>
  );
}
