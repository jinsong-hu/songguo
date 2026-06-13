import { Link, useNavigate } from 'react-router-dom';
import { ArrowLeft } from 'lucide-react';
import { Page } from '../components/Layout';
import { ProviderForm } from '../components/ProviderForm';
import { useToast } from '../components/Toast';

export function ProviderNewPage() {
  const navigate = useNavigate();
  const toast = useToast();

  return (
    <Page
      title="Add custom provider"
      actions={
        <Link to="/providers" className="btn">
          <ArrowLeft size={15} /> Back
        </Link>
      }
    >
      <p className="muted" style={{ maxWidth: 760, marginTop: 0, fontSize: 13 }}>
        Any OpenAI- or Anthropic-compatible endpoint: set the base URL, key, the wires it
        speaks, and per-model prices yourself. For a known vendor, go back and pick its tile
        instead — the endpoints come pre-filled.
      </p>
      <ProviderForm
        onCancel={() => navigate('/providers')}
        onSaved={() => {
          toast.success('Provider added.');
          navigate('/providers');
        }}
      />
    </Page>
  );
}
