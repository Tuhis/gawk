import type { ComponentPropsWithRef } from 'react';
import styles from './Button.module.css';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';

// With a ref: React 19 passes it through `rest` like any other prop.
interface Props extends ComponentPropsWithRef<'button'> {
  variant?: ButtonVariant;
}

// The production button primitive. Variants are the only place button styling
// is defined; the global <button> style is for the debug pages.
export function Button({ variant = 'primary', className, type = 'button', ...rest }: Props) {
  const cls = [styles.btn, styles[variant], className].filter(Boolean).join(' ');
  return <button type={type} className={cls} {...rest} />;
}
