/** Small dot grading how trustworthy a call's metering is. */
const CONFIDENCE_COLORS: Record<string, string> = {
  measured: 'var(--accent, #2e7d5b)',
  derived: '#d99a2b',
  unknown: 'var(--text-muted)',
};

export function ConfidenceDot({ confidence }: { confidence: string }) {
  if (!confidence) return <span style={{ color: 'var(--text-muted)' }}>—</span>;
  const color = CONFIDENCE_COLORS[confidence] ?? 'var(--text-muted)';
  return (
    <span
      title={`metering: ${confidence}`}
      style={{ display: 'inline-flex', alignItems: 'center', gap: 5, fontSize: 11.5 }}
    >
      <span
        style={{
          width: 7,
          height: 7,
          borderRadius: '50%',
          background: color,
          display: 'inline-block',
        }}
      />
      {confidence}
    </span>
  );
}
