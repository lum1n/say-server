import { cloneElement, type ReactElement, useId } from "react";

// Field labels one input; the hint describes it without becoming part of
// its accessible name.
export function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: ReactElement<{ id?: string; "aria-describedby"?: string }>;
}) {
  const id = useId();
  return (
    <div style={{ display: "grid", gap: 4, marginBottom: 12 }}>
      <label htmlFor={id}>{label}</label>
      {cloneElement(children, { id, "aria-describedby": hint ? `${id}-hint` : undefined })}
      {hint && (
        <small id={`${id}-hint`} style={{ opacity: 0.7 }}>
          {hint}
        </small>
      )}
    </div>
  );
}

export function FormError({ error }: { error: Error | null }) {
  if (!error) return null;
  return (
    <p role="alert" style={{ color: "crimson" }}>
      {error.message}
    </p>
  );
}
