// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { acceptCurrentTerms, hasAcceptedCurrentTerms, TERMS_ACCEPTED_KEY } from './acceptance';
import { BUNDLED_TERMS_VERSION } from '../../config';

describe('terms acceptance', () => {
  beforeEach(() => {
    localStorage.clear();
    delete (window as { __GAWK_CONFIG__?: unknown }).__GAWK_CONFIG__;
  });
  afterEach(() => {
    localStorage.clear();
    delete (window as { __GAWK_CONFIG__?: unknown }).__GAWK_CONFIG__;
  });

  it('starts unaccepted', () => {
    expect(hasAcceptedCurrentTerms()).toBe(false);
  });

  it('accepts the current (bundled) version', () => {
    acceptCurrentTerms();
    expect(localStorage.getItem(TERMS_ACCEPTED_KEY)).toBe(BUNDLED_TERMS_VERSION);
    expect(hasAcceptedCurrentTerms()).toBe(true);
  });

  it('re-prompts when the configured version changes (D7)', () => {
    acceptCurrentTerms(); // stores the bundled version
    expect(hasAcceptedCurrentTerms()).toBe(true);
    // Operator publishes new terms with a bumped version.
    window.__GAWK_CONFIG__ = { termsVersion: '2027-01-01' };
    expect(hasAcceptedCurrentTerms()).toBe(false);
    acceptCurrentTerms();
    expect(hasAcceptedCurrentTerms()).toBe(true);
    expect(localStorage.getItem(TERMS_ACCEPTED_KEY)).toBe('2027-01-01');
  });

  it('a stale acceptance for a different version does not count', () => {
    localStorage.setItem(TERMS_ACCEPTED_KEY, 'some-old-version');
    expect(hasAcceptedCurrentTerms()).toBe(false);
  });
});
