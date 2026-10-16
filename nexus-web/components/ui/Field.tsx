"use client";

// Field — label + control + error/hint block (13px form rhythm, Phase 5b). Recipe = the
// hand-rolled form blocks in CreateAppDialog: label 12px font-medium --mut · error 11.5px
// --dgf · hint 11.5px --fnt. Error REPLACES the hint when present (one helper line only).
// Pass htmlFor + a matching id on the control — Field stays dumb on purpose (no cloning).

export function Field({
  label,
  htmlFor,
  required = false,
  error,
  hint,
  className,
  children,
}: {
  label: React.ReactNode;
  htmlFor?: string;
  required?: boolean;
  error?: React.ReactNode;
  hint?: React.ReactNode;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={className}>
      <label htmlFor={htmlFor} className="mb-1 block text-sub font-medium text-mut">
        {label}
        {required && <span className="text-dgf"> *</span>}
      </label>
      {children}
      {error ? (
        <p className="mt-1 text-11-5px text-dgf">{error}</p>
      ) : hint ? (
        <p className="mt-1 text-11-5px text-fnt">{hint}</p>
      ) : null}
    </div>
  );
}
