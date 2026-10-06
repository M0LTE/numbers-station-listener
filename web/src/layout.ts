// The one breakpoint the scripts care about: below it the schedule is a
// compact list and the player is a full-screen sheet opened from a dock at
// the bottom of the screen. Keep in step with style.css.

export const narrowMq: MediaQueryList = window.matchMedia("(max-width: 959px)");

export function isNarrow(): boolean {
  return narrowMq.matches;
}

/**
 * Whether scripts can set the media volume. iOS keeps volume for the
 * hardware buttons and ignores the property, so read it back.
 */
export const volumeWorks: boolean = (() => {
  try {
    const a = document.createElement("audio");
    a.volume = 0.5;
    return Math.abs(a.volume - 0.5) < 0.01;
  } catch {
    return false;
  }
})();
