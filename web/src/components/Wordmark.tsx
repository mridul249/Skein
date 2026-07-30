import clsx from 'clsx';

/**
 * The wordmark: lowercase `skein`, JetBrains Mono 700, behind a mauve chevron.
 *
 *     › skein
 *
 * Lowercase because the whole identity is a terminal one and a capitalised
 * product name in a monospace face reads as a heading rather than as a prompt.
 * The chevron is the in-app treatment — it is what makes the name read as a
 * shell prompt rather than as a logo, and it is the only decorative mark in the
 * interface, which is why it is allowed to be the accent colour.
 *
 * It is a component rather than three copies of the same span because the
 * header, the sign-in screen and the boot splash all render it, and a wordmark
 * that drifts between screens is worse than no wordmark.
 */
export function Wordmark({
  className,
  chevron = true,
}: {
  className?: string;
  /** Off where the mark already supplies the prompt, e.g. beside the logo. */
  chevron?: boolean;
}) {
  return (
    <span className={clsx('font-bold lowercase text-text', className)}>
      {chevron && (
        // Decorative: the accessible name comes from the word itself, and a
        // screen reader announcing "single right angle quotation mark skein"
        // would be worse than silence.
        <span className="text-mauve" aria-hidden>
          ›{' '}
        </span>
      )}
      skein
    </span>
  );
}
