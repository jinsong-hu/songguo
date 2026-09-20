import { ChevronRight } from 'lucide-react';
import type { SystemOneAnswer, SystemOneCall, SystemOneQuestion } from '../api/types';
import { ErrorBanner } from './ErrorBanner';
import { Skeleton } from './Skeleton';
import styles from '../pages/Detail.module.css';

// The State / Questions & answers panels for a typesafe/systemone call, from
// GET /api/calls/{id}/systemone. It stands where PromptReconstructionCard
// stands on every other wire: a System One body carries no system, tools or
// messages, so that card can only ever render three empty panels for it.
export function SystemOnePanel({
  call,
  loading,
  error,
  onRetry,
}: {
  call: SystemOneCall | null;
  loading: boolean;
  error?: string | null;
  onRetry: () => void;
}) {
  if (error) return <ErrorBanner message={error} onRetry={onRetry} />;

  if (loading || !call) {
    return (
      <div className={styles.promptGrid}>
        <Skeleton height={140} />
        <Skeleton height={220} />
      </div>
    );
  }

  const rows = pairRows(call.questions, call.answers);

  return (
    <div className={styles.promptGrid}>
      <details className={styles.promptPanel} open>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            State
          </span>
          <span className="chip chip-mono">{call.state.length} chars</span>
        </summary>
        <div className={styles.promptScroll}>
          {call.state ? (
            <pre className={styles.promptTextBlock}>{call.state}</pre>
          ) : (
            <div className={styles.promptEmpty}>No state in this request.</div>
          )}
        </div>
      </details>

      <details className={styles.promptPanel} open>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            Questions &amp; answers
          </span>
          <span className="chip chip-mono">{rows.length}</span>
        </summary>
        {rows.length > 0 ? (
          <div className={styles.soList}>
            {rows.map((row) => (
              <QARow key={row.key} row={row} />
            ))}
          </div>
        ) : (
          <div className={styles.promptEmpty}>No questions captured.</div>
        )}
      </details>
    </div>
  );
}

interface QAPair {
  key: string;
  question: SystemOneQuestion | null;
  answer: SystemOneAnswer | null;
}

// pairRows joins the two maps on their shared key. A question with no answer,
// and an answer key with no question, are both rendered rather than dropped:
// each is a real observation about the call, and silently discarding either
// would make an incomplete response look like a complete one.
function pairRows(questions: SystemOneQuestion[], answers: SystemOneAnswer[]): QAPair[] {
  const byKey = new Map<string, QAPair>();
  for (const question of questions) {
    byKey.set(question.key, { key: question.key, question, answer: null });
  }
  for (const answer of answers) {
    const row = byKey.get(answer.key);
    if (row) row.answer = answer;
    else byKey.set(answer.key, { key: answer.key, question: null, answer });
  }
  return [...byKey.values()];
}

function QARow({ row }: { row: QAPair }) {
  const type = row.question?.type || row.answer?.type || '';
  return (
    <div className={styles.soRow}>
      <div className={styles.soRowHead}>
        <span className={`${styles.soKey} mono`}>{row.key}</span>
        {type ? <span className={styles.soTypeChip}>{type}</span> : null}
      </div>

      {row.question ? (
        row.question.instructions ? (
          <p className={styles.soInstructions}>{row.question.instructions}</p>
        ) : (
          <p className={styles.soMuted}>No instructions on this question.</p>
        )
      ) : (
        <p className={styles.soMuted}>Answered, but this key was not in the request's questions.</p>
      )}

      {row.answer ? (
        <AnswerValue answer={row.answer} />
      ) : (
        <p className={styles.soMuted}>No answer for this question in the response.</p>
      )}

      <RawDisclosure question={row.question} answer={row.answer} />
    </div>
  );
}

// AnswerValue renders the typed value per its declared type: noul and score as
// a bar plus the number, choice as the chosen label. An answer whose type names
// no value the backend could read falls back to naming that, with the raw JSON
// a disclosure away.
function AnswerValue({ answer }: { answer: SystemOneAnswer }) {
  if (answer.value === undefined || answer.value === null) {
    return <p className={styles.soMuted}>No value at "{answer.type || 'type'}" in the answer.</p>;
  }
  if ((answer.type === 'noul' || answer.type === 'score') && typeof answer.value === 'number') {
    return <ProbabilityBar value={answer.value} />;
  }
  if (answer.type === 'choice' && typeof answer.value === 'string') {
    return (
      <div className={styles.soAnswer}>
        <span className={styles.soChoice}>{answer.value}</span>
      </div>
    );
  }
  // A shape this build does not render: show the value rather than nothing.
  return (
    <div className={styles.soAnswer}>
      <span className={`${styles.soChoice} mono`}>{JSON.stringify(answer.value)}</span>
    </div>
  );
}

// A noul is a probability in [0,1]; a score is graded and may run past 1. The
// bar is clamped so an out-of-range value cannot draw outside its track, and
// the number beside it is always the value as reported.
function ProbabilityBar({ value }: { value: number }) {
  const pct = Math.max(0, Math.min(1, value)) * 100;
  return (
    <div className={styles.soAnswer}>
      <div className={styles.soBarTrack}>
        <div className={styles.soBarFill} style={{ width: `${pct}%` }} />
      </div>
      <span className={`${styles.soValue} mono`}>{formatValue(value)}</span>
    </div>
  );
}

// Three decimals suits a calibrated probability. A value smaller than that is
// printed as reported rather than rounded to a flat "0" — the difference
// between "the model said no" and "the model said almost no" is the answer.
function formatValue(value: number): string {
  if (Number.isInteger(value)) return String(value);
  const fixed = value.toFixed(3).replace(/0+$/, '').replace(/\.$/, '');
  return Number(fixed) === 0 ? String(value) : fixed;
}

// The verbatim question and answer objects. They are kept so a field TypeSafe
// reports that this build does not name — a confidence, a rationale — is still
// reachable here instead of only in the raw trace below.
function RawDisclosure({
  question,
  answer,
}: {
  question: SystemOneQuestion | null;
  answer: SystemOneAnswer | null;
}) {
  const raw: Record<string, unknown> = {};
  if (question?.raw !== undefined) raw.question = question.raw;
  if (answer?.raw !== undefined) raw.answer = answer.raw;
  if (Object.keys(raw).length === 0) return null;
  return (
    <details className={styles.soRaw}>
      <summary className={styles.soRawHead}>Raw JSON</summary>
      <pre className={styles.soRawBody}>{JSON.stringify(raw, null, 2)}</pre>
    </details>
  );
}
