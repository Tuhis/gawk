import type { ButtonHTMLAttributes, ReactNode } from 'react';
import styles from './IconButton.module.css';

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  // Required: icon-only buttons must carry an accessible name.
  label: string;
  children: ReactNode;
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
