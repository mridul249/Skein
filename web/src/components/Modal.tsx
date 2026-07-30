import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import clsx from 'clsx';

/**
 * The one dialog. Design.md §7 and §8.
 *
 * This replaces `window.confirm` and `window.prompt`, which were the last
 * places the interface handed the user to the browser chrome. Three reasons
 * that mattered enough to build this: a native dialog cannot be styled, so it
 * broke the terminal identity the moment it opened; it cannot carry the
 * per-drive colour or a monospace byte count, so §7's "name the consequence"
 * wording had nowhere to live; and `confirm()` blocks the event loop, which
 * means an in-flight upload's progress stops repainting while it is open.
 *
 * Keyboard operation is not a bonus here. Design.md §8 lists a shortcut for
 * every primary action, so a dialog that can only be dismissed with a mouse
 * contradicts the model the rest of the app teaches.
 */

/** How the confirm button is styled, and therefore how it reads. */
export type Intent = 'primary' | 'danger';

interface ModalProps {
  open: boolean;
  title: string;
  /** The body. Prose, not a heading — the heading is `title`. */
  children?: ReactNode;
  confirmLabel: string;
  cancelLabel?: string;
  intent?: Intent;
  /**
   * When set, the dialog carries a single text field and `onConfirm` receives
   * its value. This is the `window.prompt` replacement.
   */
  prompt?: { label: string; placeholder?: string; initial?: string };
  onConfirm: (value: string) => void;
  onCancel: () => void;
}

/** Everything focusable, in tab order, inside a container. */
function focusable(root: HTMLElement): HTMLElement[] {
  return Array.from(
    root.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    ),
  ).filter((el) => el.offsetParent !== null || el === document.activeElement);
}

export function Modal({
  open,
  title,
  children,
  confirmLabel,
  cancelLabel = 'Cancel',
  intent = 'primary',
  prompt,
  onConfirm,
  onCancel,
}: ModalProps) {
  const panelRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  // Captured on open, restored on close. Without this, dismissing the dialog
  // drops focus to <body> and a keyboard user loses their place in the table.
  const returnTo = useRef<HTMLElement | null>(null);

  const titleId = useId();
  const bodyId = useId();
  const [value, setValue] = useState(prompt?.initial ?? '');

  // Reset the field each time the dialog opens, or the previous folder name
  // is still sitting there the next time it is used.
  useEffect(() => {
    if (open) setValue(prompt?.initial ?? '');
  }, [open, prompt?.initial]);

  useEffect(() => {
    if (!open) return;

    returnTo.current = document.activeElement as HTMLElement | null;

    // A prompt wants the field. A confirmation gives focus to Cancel, never
    // to the destructive button: the dialog exists to interrupt, and opening
    // with the irreversible action one Enter away defeats that.
    const initial = prompt ? inputRef.current : intent === 'danger' ? cancelRef.current : confirmRef.current;
    initial?.focus();

    return () => {
      // Restore on close. Guard against the trigger having been unmounted by
      // whatever the dialog just did — focusing a detached node throws away
      // focus entirely.
      const target = returnTo.current;
      if (target && document.contains(target)) target.focus();
    };
  }, [open, prompt, intent]);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        onCancel();
        return;
      }
      if (e.key !== 'Tab') return;

      // The trap. Without it, Tab walks out of the dialog and into the page
      // behind it, which is still rendered and still clickable to a screen
      // reader that ignores the overlay.
      const items = panelRef.current ? focusable(panelRef.current) : [];
      if (items.length === 0) return;

      const first = items[0]!;
      const last = items[items.length - 1]!;
      const active = document.activeElement;

      if (e.shiftKey && (active === first || !panelRef.current?.contains(active))) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    },
    [onCancel],
  );

  if (!open) return null;

  return (
    <div
      // Design.md §2: no shadows. The dim is what separates the dialog from
      // the page, and it is the only place a translucent layer is used.
      className="fixed inset-0 z-50 flex items-center justify-center bg-crust/80 p-2ch"
      onMouseDown={(e) => {
        // Only a press that both starts and ends on the backdrop dismisses,
        // so a drag that ends outside a text selection does not.
        if (e.target === e.currentTarget) onCancel();
      }}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={children ? bodyId : undefined}
        onKeyDown={onKeyDown}
        className={clsx(
          // `min-w-0` is required, not tidiness: as a flex child the panel
          // defaults to `min-width: auto`, i.e. min-content, and a 66-character
          // filename in the heading forced it to 562px inside a 375px viewport.
          // `break-words` alone does not fix this — `overflow-wrap: break-word`
          // wraps the text but does not reduce the intrinsic min-content width
          // the flex floor is computed from.
          'w-full min-w-0 max-w-[64ch] border border-surface0 bg-base p-2ch',
          // Design.md §6: 180ms on panel open, and nothing at all when the
          // viewer has asked for less motion.
          'motion-safe:animate-modal-in',
        )}
      >
        {/*
          `break-words` is load-bearing, not defensive. Every title here
          interpolates a user-supplied identifier — an email or a filename —
          which is a single unbroken token with no spaces to wrap at. In a
          monospace face at 375px that runs straight out of the panel. Applies
          to the body too, which carries the same values.
        */}
        <h2 id={titleId} className="break-words text-heading font-bold text-text">
          {title}
        </h2>

        {children && (
          <div id={bodyId} className="mt-1line break-words text-body text-subtext0">
            {children}
          </div>
        )}

        {prompt && (
          <div className="mt-1line">
            <label htmlFor={`${titleId}-field`} className="label">
              {prompt.label}
            </label>
            <input
              id={`${titleId}-field`}
              ref={inputRef}
              className="input"
              value={value}
              placeholder={prompt.placeholder}
              onChange={(e) => setValue(e.target.value)}
              onKeyDown={(e) => {
                // Enter submits, so the field behaves the way the native
                // prompt it replaces did.
                if (e.key === 'Enter' && value.trim()) {
                  e.preventDefault();
                  onConfirm(value.trim());
                }
              }}
            />
          </div>
        )}

        <div className="mt-1line flex justify-end gap-1ch">
          <button type="button" ref={cancelRef} className="btn-ghost" onClick={onCancel}>
            {cancelLabel}
          </button>
          <button
            type="button"
            ref={confirmRef}
            className={intent === 'danger' ? 'btn-danger' : 'btn-primary'}
            disabled={Boolean(prompt) && !value.trim()}
            onClick={() => onConfirm(value.trim())}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
