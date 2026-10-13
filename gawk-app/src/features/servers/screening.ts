// A server's content screening (R40, docs/71 D13): one quiet mark among the
// server's details, shown only when its probe says it screens. Never on the
// broadcast screen, and never a mark that a server doesn't screen: that
// would point the people screening exists for at the servers without it.
// The same words on every broadcaster surface — this app, the desktop window
// and the iOS app — so a broadcaster reads one claim, not three.
export const SCREENING_LABEL = 'Content screening';
// What the mark means, as its tooltip.
export const SCREENING_NOTICE = 'This server automatically screens streams for explicit content.';
