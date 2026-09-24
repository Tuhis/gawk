import type { ButtonHTMLAttributes } from 'react';
import styles from './Button.module.css';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
}

// The production button primitive. Variants are the only place button styling
// is defined; the global <button> style is for the debug pages.
export function Button({ variant = 'primary', className, type = 'button', ...rest }: Props) {
  const cls = [styles.btn, styles[variant], className].filter(Boolean).join(' ');
  return <button type={type} className={cls} {...rest} />;
}
