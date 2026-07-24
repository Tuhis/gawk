// R23 (docs/29 §4.4). The broadcaster acknowledges the terms once before their
// first broadcast; acceptance is stored client-side (there is no identity to
// bind it to — the no-account model stands). The stored value is the version
// string that was agreed, so bumping config.termsVersion re-prompts everyone
// (D7). Viewers are never gated and never touch this.
import { getTermsVersion } from '../../config';

// Follows the existing gawk:* localStorage convention.
export const TERMS_ACCEPTED_KEY = 'gawk:terms-accepted';

export function hasAcceptedCurrentTerms(): boolean {
  try {
    return localStorage.getItem(TERMS_ACCEPTED_KEY) === getTermsVersion();
  } catch {
    // Private-mode / disabled storage: fail closed (re-prompt) rather than
    // throwing on the broadcast path.
    return false;
  }
}

export function acceptCurrentTerms(): void {
  try {
    localStorage.setItem(TERMS_ACCEPTED_KEY, getTermsVersion());
  } catch {
    // Nothing to persist to; the acknowledgment still lets this session start,
    // it will simply re-prompt next time.
  }
}
