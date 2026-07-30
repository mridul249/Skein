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
 */
export function Tooltip({
  content,
  children,
  disabled = false,
  className,
}: {
  content: ReactNode;
  children: ReactNode;
  /** Skips the tooltip entirely, for the compact sidebar rail. */
  disabled?: boolean;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const id = useId();

  if (disabled) return <>{children}</>;

  return (
    <div
      className={clsx('relative', className)}
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
      aria-describedby={open ? id : undefined}
    >
      {children}

      {open && (
        <div
          id={id}
          role="tooltip"
          className="pointer-events-none absolute bottom-full left-1/2 z-30 mb-2 w-max max-w-xs
                     -translate-x-1/2 border border-surface0 bg-base px-1ch py-halfline
                     text-left shadow-none"
        >
          {content}
        </div>
      )}
    </div>
  );
}
