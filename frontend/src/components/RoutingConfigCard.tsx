import type { CSSProperties, ReactNode } from 'react';
import { InfoHint } from './InfoHint';
import styles from './RoutingConfigCard.module.css';

export interface RoutingConfigItem {
  id: string;
  name: string;
  icon?: ReactNode;
  color: string;
  enabled: boolean;
  available: boolean;
  custom?: boolean;
  priority: string;
  weight: string;
  defaultPriority?: number;
  defaultWeight?: number;
  unavailableLabel?: string;
}

interface RoutingConfigCardProps {
  title: string;
  hint: string;
  items: RoutingConfigItem[];
  saving?: boolean;
  dirty?: boolean;
  editableEnabled?: boolean;
  inherited?: boolean;
  onChange?: (id: string, patch: Partial<RoutingConfigItem>) => void;
  onSave?: () => void;
}

function numeric(value: string, fallback: number): number {
  const n = Number(value);
  return Number.isFinite(n) ? n : fallback;
}

export function RoutingConfigCard({
  title,
  hint,
  items,
  saving = false,
  dirty = false,
  editableEnabled = false,
  inherited = false,
  onChange,
  onSave,
}: RoutingConfigCardProps) {
  const active = items
    .filter((item) => item.enabled && item.available)
    .map((item) => ({
      ...item,
      priority: Math.max(0, numeric(item.priority, item.defaultPriority ?? 0)),
      weight: Math.max(1, numeric(item.weight, item.defaultWeight ?? 1)),
    }));
  const priorities = [...new Set(active.map((item) => item.priority))].sort((a, b) => a - b);
  const inactive = items.filter((item) => !item.enabled || !item.available);

  return (
    <section className={`card ${styles.card}`}>
      <div className={styles.head}>
        <div className={styles.titleRow}>
          <h2 className={styles.title}>{title}</h2>
          <InfoHint text={hint} label={`About ${title}`} />
        </div>
        {onSave && (
          <button
            type="button"
            className="btn btn-primary btn-sm"
            disabled={!dirty || saving}
            onClick={onSave}
          >
            {saving ? <span className="spinner" /> : null}
            {saving ? 'Saving' : 'Save'}
          </button>
        )}
      </div>

      <div className={styles.layers} aria-label={`${title} priority layers`}>
        {priorities.length === 0 ? (
          <div className={styles.empty}>No active providers</div>
        ) : (
          priorities.map((priority, index) => {
            const layer = active.filter((item) => item.priority === priority);
            const total = layer.reduce((sum, item) => sum + item.weight, 0);
            return (
              <div key={priority} className={styles.layer}>
                <div className={styles.layerMeta}>
                  <span className={styles.priority}>Priority {priority}</span>
                  <span>{index === 0 ? 'Active' : `Fallback ${index}`}</span>
                </div>
                <div
                  className={styles.segments}
                  style={{ gridTemplateColumns: layer.map((item) => `${item.weight}fr`).join(' ') }}
                >
                  {layer.map((item) => {
                    const share = Math.round((item.weight / total) * 100);
                    return (
                      <div
                        key={item.id}
                        className={styles.segment}
                        style={{ '--routing-color': item.color } as CSSProperties}
                        title={`${item.name}: weight ${item.weight}, approximately ${share}% of new sessions in this priority`}
                      >
                        <span className={styles.segmentIcon}>{item.icon}</span>
                        <span className={styles.segmentName}>{item.name}</span>
                        <span className={styles.share}>{share}%</span>
                      </div>
                    );
                  })}
                </div>
              </div>
            );
          })
        )}
      </div>

      {inactive.length > 0 && (
        <div className={styles.inactive}>
          {inactive.map((item) => (
            <span key={item.id} className={styles.inactiveItem}>
              {item.icon}
              <span>{item.name}</span>
              <span className={styles.inactiveReason}>
                {item.enabled ? item.unavailableLabel ?? 'Unavailable' : 'Disabled'}
              </span>
            </span>
          ))}
        </div>
      )}

      {onChange && (
        <div
          className={`${styles.editor} ${
            !editableEnabled && !inherited ? styles.compactEditor : ''
          }`}
        >
          <div className={styles.editorHead} aria-hidden="true">
            <span>Provider</span>
            {editableEnabled && <span>Enabled</span>}
            {inherited && <span>Policy</span>}
            <span>Priority</span>
            <span>Weight</span>
          </div>
          {items.map((item) => {
            const fieldsDisabled = inherited && !item.custom;
            return (
              <div key={item.id} className={styles.editorRow}>
                <div className={styles.provider}>
                  <span className={styles.providerIcon}>{item.icon}</span>
                  <span className={styles.providerName}>{item.name}</span>
                </div>
                {editableEnabled && (
                  <label className={styles.switch}>
                    <input
                      type="checkbox"
                      checked={item.enabled}
                      onChange={(event) => onChange(item.id, { enabled: event.target.checked })}
                    />
                    <span aria-hidden="true" />
                    <span className="sr-only">
                      {item.enabled ? `Disable ${item.name}` : `Enable ${item.name}`}
                    </span>
                  </label>
                )}
                {inherited && (
                  <div className={styles.mode} aria-label={`${item.name} routing policy`}>
                    <button
                      type="button"
                      className={!item.custom ? styles.modeActive : ''}
                      onClick={() =>
                        onChange(item.id, {
                          custom: false,
                          priority: String(item.defaultPriority ?? 0),
                          weight: String(item.defaultWeight ?? 1),
                        })
                      }
                    >
                      Default
                    </button>
                    <button
                      type="button"
                      className={item.custom ? styles.modeActive : ''}
                      onClick={() => onChange(item.id, { custom: true })}
                    >
                      Custom
                    </button>
                  </div>
                )}
                <label className={styles.numberField}>
                  <span className="sr-only">{item.name} priority</span>
                  <span className={styles.mobileLabel}>Priority</span>
                  <input
                    className="input"
                    type="number"
                    min={0}
                    step={1}
                    value={item.priority}
                    disabled={fieldsDisabled}
                    onChange={(event) => onChange(item.id, { priority: event.target.value })}
                  />
                </label>
                <label className={styles.numberField}>
                  <span className="sr-only">{item.name} weight</span>
                  <span className={styles.mobileLabel}>Weight</span>
                  <input
                    className="input"
                    type="number"
                    min={1}
                    step={1}
                    value={item.weight}
                    disabled={fieldsDisabled}
                    onChange={(event) => onChange(item.id, { weight: event.target.value })}
                  />
                </label>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}
