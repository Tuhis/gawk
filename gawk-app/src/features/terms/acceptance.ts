// Broadcasters acknowledge the terms once before their first broadcast. There
// is no identity to bind that to, so the acknowledged version is stored
// client-side; bumping config.termsVersion re-prompts everyone. Unreadable
// storage fails closed: the prompt shows again.
import { getTermsVersion } from '../../config';
import { readStored, writeStored } from '../../lib/storage';

export const TERMS_ACCEPTED_KEY = 'gawk:terms-accepted';

export function hasAcceptedCurrentTerms(): boolean {
  return readStored(TERMS_ACCEPTED_KEY) === getTermsVersion();
}

export function acceptCurrentTerms(): void {
  writeStored(TERMS_ACCEPTED_KEY, getTermsVersion());
}
