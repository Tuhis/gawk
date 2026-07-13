import { useCallback, useEffect, useState, type RefObject } from 'react';

// Fullscreen for a target element (docs/10 J3). Tracks the real fullscreen
// state via the `fullscreenchange` event so the UI stays correct when the user
// presses Esc (which the browser handles) rather than the button.
export function useFullscreen(ref: RefObject<HTMLElement | null>) {
  const [isFullscreen, setIsFullscreen] = useState(
    typeof document !== 'undefined' && document.fullscreenElement != null,
  );

  useEffect(() => {
    const onChange = () => setIsFullscreen(document.fullscreenElement != null);
    document.addEventListener('fullscreenchange', onChange);
    return () => document.removeEventListener('fullscreenchange', onChange);
  }, []);

  const toggle = useCallback(() => {
    if (document.fullscreenElement) {
      void document.exitFullscreen?.();
    } else {
      void ref.current?.requestFullscreen?.();
    }
  }, [ref]);

  return { isFullscreen, toggle };
}
