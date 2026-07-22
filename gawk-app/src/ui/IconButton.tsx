import type { ButtonHTMLAttributes, ReactNode, Ref } from 'react';
import styles from './IconButton.module.css';

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  // Required: icon-only buttons must carry an accessible name.
  label: string;
  children: ReactNode;
  // React 19 passes refs as ordinary props; declared so callers that need the
  // element (menu anchoring) can reach it.
  ref?: Ref<HTMLButtonElement>;
}

export function IconButton({ label, className, children, type = 'button', ...rest }: Props) {
  return (
    <button
      type={type}
      aria-label={label}
      title={label}
      className={[styles.iconBtn, className].filter(Boolean).join(' ')}
      {...rest}
    >
      {children}
    </button>
  );
}
