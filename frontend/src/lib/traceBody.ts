/**
 * traceBody prepares a captured payload for display in the trace panel.
 *
 * This is a *display* transform and nothing more. The wire is never touched
 * (see CLAUDE.md), the stored bytes are never touched, and the trace panel's
 * Copy button still yields the untouched `side.body` — so anything shortened
 * here is always one click away in full.
 *
 * Why it has to exist: an image call captures its result as base64 inside the
 * JSON, so a single `"b64_json": "…"` value can run to several million
 * characters. Rendered into a `white-space: pre` block that is one line, and
 * Chrome stops painting a text run once its line box passes 2^24 CSS px —
 * about 2.4M monospace characters. The line still lays out and still takes
 * vertical space; it just comes out blank, taking its neighbours with it, so
 * the pane shows a hole where the response should be.
 *
 * Wrapping the line instead is worse, not better: 3 MB of base64 wraps to a
 * 305,000px-tall scroll region inside a 320px window, which buries the rest of
 * the body thousands of screens down. And readability fails long before either
 * limit — at 10,000 characters the line is already 69,400px wide, of which
 * about 110 characters are ever on screen.
 *
 * So we shorten long *values* and report how much we took out; the caller
 * shows that count beside the pane. The limit is set for legibility rather
 * than for the paint cliff, which is three orders of magnitude further out.
 */

/**
 * Longest string value rendered in full.
 *
 * Chosen for the pane, not for the cliff: the prefix is there to identify the
 * value, and past a couple of thousand characters a non-wrapping 320px pane
 * cannot show it anyway — it only stretches the pane's horizontal scrollbar
 * into a hairline. The cost is that prose longer than this (a big system
 * prompt) now stops at the marker instead of running off to the right; Copy
 * still yields it whole. If that becomes the thing people miss, the answer is
 * to let `.bodyCode` wrap — which is safe once blobs are shortened, and is
 * what makes long prompts readable rather than merely present — not to raise
 * this number.
 */
export const MAX_VALUE_CHARS = 2048;

export interface DisplayBody {
  /** The text to render. */
  text: string;
  /** How many characters were left out of `text`; 0 when nothing was. */
  elided: number;
}

/** Replace the tail of an over-long string with a count of what was dropped. */
function elide(s: string): string {
  const dropped = s.length - MAX_VALUE_CHARS;
  return `${s.slice(0, MAX_VALUE_CHARS)}… [${dropped.toLocaleString()} more characters — Copy for the full body]`;
}

/**
 * displayBody renders one captured side: pretty-printed when the body is JSON,
 * verbatim otherwise, with over-long values shortened either way.
 */
export function displayBody(body: string, base64 = false): DisplayBody {
  // A binary body arrives as one base64 blob with no structure to walk, so it
  // takes the line path directly rather than being parsed as JSON first.
  if (base64) return elideLines(body);

  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    // Not JSON — a raw text body, or an SSE stream, where a single `data:`
    // frame can still carry a blob. Shorten per line.
    return elideLines(body);
  }

  let elided = 0;
  const text = JSON.stringify(
    parsed,
    (_key, value) => {
      if (typeof value === 'string' && value.length > MAX_VALUE_CHARS) {
        elided += value.length - MAX_VALUE_CHARS;
        return elide(value);
      }
      return value;
    },
    2,
  );
  // JSON.stringify returns undefined for a body that parsed to undefined; that
  // cannot come from valid JSON, but the type says it can, so fall back.
  return { text: text ?? body, elided };
}

/** Shorten any single line that is too long, leaving the rest untouched. */
function elideLines(body: string): DisplayBody {
  if (body.length <= MAX_VALUE_CHARS) return { text: body, elided: 0 };

  let elided = 0;
  const lines = body.split('\n').map((line) => {
    if (line.length <= MAX_VALUE_CHARS) return line;
    elided += line.length - MAX_VALUE_CHARS;
    return elide(line);
  });
  return { text: lines.join('\n'), elided };
}
