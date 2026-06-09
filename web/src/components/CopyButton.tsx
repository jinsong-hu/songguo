import { useState } from 'react';
import { Check, Copy } from 'lucide-react';

interface CopyButtonProps {
  value: string;
  /** Optional label rendered next to the icon. */
  label?: string;
  className?: string;
}

/** A small button that copies a value to the clipboard and confirms briefly. */
export function CopyButton({ value, label, className }: CopyButtonProps) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Fallback for non-secure contexts.
      const ta = document.createElement('textarea');
      ta.value = value;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1400);
  };

  return (
    <button
      type="button"
      className={`btn btn-sm ${className ?? ''}`}
      onClick={copy}
      aria-label={`Copy ${label ?? 'value'}`}
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}
      {label ? <span>{copied ? 'Copied' : label}</span> : null}
    </button>
  );
}
