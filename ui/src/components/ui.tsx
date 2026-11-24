// Styled wrappers over react-aria-components. Styling lives in ui.css; DESIGN.md lists the inventory.
// Components carry no user-visible copy of their own: labels come from props or strings.ts.
import type { ReactNode } from "react";
import * as RAC from "react-aria-components";
import { strings } from "../strings";
import "./ui.css";

const cx = (...parts: (string | false | null | undefined)[]) => parts.filter(Boolean).join(" ");

/* Icons: one authored set, 24 px grid, 1.75 stroke, round caps. Always decorative. */

const iconPaths = {
  status: "M4 12h3l2.5-6 5 12 2.5-6h3",
  print:
    "M7 9V3.75h10V9M7 17H5a1.5 1.5 0 0 1-1.5-1.5v-5A1.5 1.5 0 0 1 5 9h14a1.5 1.5 0 0 1 1.5 1.5v5A1.5 1.5 0 0 1 19 17h-2M7 14h10v6.25H7z",
  scan: "M3.5 8V5A1.5 1.5 0 0 1 5 3.5h3M16 3.5h3A1.5 1.5 0 0 1 20.5 5v3M20.5 16v3a1.5 1.5 0 0 1-1.5 1.5h-3M8 20.5H5A1.5 1.5 0 0 1 3.5 19v-3M3 12h18",
  settings: "M4 7h9M17 7h3M4 17h3M11 17h9M15 4.5v5M9 14.5v5",
  chevronDown: "m6 9 6 6 6-6",
  check: "m5 12.5 4.5 4.5L19 7.5",
  plus: "M12 5v14M5 12h14",
  minus: "M5 12h14",
  close: "M6 6l12 12M18 6 6 18",
  alert:
    "M12 8v5M12 16.5v.01M10.3 3.9 2.6 17.3A2 2 0 0 0 4.3 20.3h15.4a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z",
  offline:
    "M3 3l18 18M8.5 16.4a5 5 0 0 1 7 0M5 12.9a10 10 0 0 1 4.2-2.5M12 8.4a10 10 0 0 1 7 4.5M2 9.3A15 15 0 0 1 6.3 6.6M10.7 4.6A15 15 0 0 1 22 9.3M12 20h.01",
  upload: "M12 15V4M7.5 8.5 12 4l4.5 4.5M4 15v3.5A1.5 1.5 0 0 0 5.5 20h13a1.5 1.5 0 0 0 1.5-1.5V15",
  download:
    "M12 4v11M7.5 10.5 12 15l4.5-4.5M4 15v3.5A1.5 1.5 0 0 0 5.5 20h13a1.5 1.5 0 0 0 1.5-1.5V15",
  copy: "M9 9h10.5v10.5H9zM15 9V4.5H4.5V15H9",
  file: "M14 3.5H7A1.5 1.5 0 0 0 5.5 5v14A1.5 1.5 0 0 0 7 20.5h10a1.5 1.5 0 0 0 1.5-1.5V8L14 3.5zM14 3.5V8h4.5",
} as const;

export type IconName = keyof typeof iconPaths;

export function Icon({
  name,
  size = 20,
  className,
}: {
  name: IconName;
  size?: number;
  className?: string;
}) {
  return (
    <svg
      className={cx("ui-icon", className)}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <path d={iconPaths[name]} />
    </svg>
  );
}

/* Buttons and links */

export type ButtonVariant = "primary" | "secondary" | "quiet" | "danger";

export type ButtonProps = Omit<RAC.ButtonProps, "className"> & {
  variant?: ButtonVariant;
  className?: string;
};

export function Button({ variant = "secondary", className, ...props }: ButtonProps) {
  return <RAC.Button {...props} className={cx("ui-button", className)} data-variant={variant} />;
}

export type LinkButtonProps = Omit<RAC.LinkProps, "className"> & {
  variant?: ButtonVariant;
  className?: string;
};

/** A link styled as a button (navigation or downloads, never actions). */
export function LinkButton({ variant = "secondary", className, ...props }: LinkButtonProps) {
  return <RAC.Link {...props} className={cx("ui-button", className)} data-variant={variant} />;
}

/* Fields */

type FieldChrome = {
  label: ReactNode;
  description?: ReactNode;
  /** Shown below the field and marks it invalid. */
  errorMessage?: string;
  className?: string;
};

function FieldFooter({
  description,
  errorMessage,
}: Pick<FieldChrome, "description" | "errorMessage">) {
  return (
    <>
      {description && (
        <RAC.Text slot="description" className="ui-field-description">
          {description}
        </RAC.Text>
      )}
      <RAC.FieldError className="ui-field-error">{errorMessage}</RAC.FieldError>
    </>
  );
}

export type TextFieldProps = Omit<RAC.TextFieldProps, "className"> &
  FieldChrome & { placeholder?: string };

export function TextField({
  label,
  description,
  errorMessage,
  placeholder,
  className,
  ...props
}: TextFieldProps) {
  return (
    <RAC.TextField
      {...props}
      isInvalid={props.isInvalid || Boolean(errorMessage)}
      className={cx("ui-field", className)}
    >
      <RAC.Label className="ui-label">{label}</RAC.Label>
      <RAC.Input className="ui-input" placeholder={placeholder} />
      <FieldFooter description={description} errorMessage={errorMessage} />
    </RAC.TextField>
  );
}

export type NumberFieldProps = Omit<RAC.NumberFieldProps, "className"> & FieldChrome;

export function NumberField({
  label,
  description,
  errorMessage,
  className,
  ...props
}: NumberFieldProps) {
  return (
    <RAC.NumberField
      {...props}
      isInvalid={props.isInvalid || Boolean(errorMessage)}
      className={cx("ui-field", className)}
    >
      <RAC.Label className="ui-label">{label}</RAC.Label>
      <RAC.Group className="ui-number">
        <RAC.Button slot="decrement" className="ui-number-step">
          <Icon name="minus" size={18} />
        </RAC.Button>
        <RAC.Input className="ui-input tabular" />
        <RAC.Button slot="increment" className="ui-number-step">
          <Icon name="plus" size={18} />
        </RAC.Button>
      </RAC.Group>
      <FieldFooter description={description} errorMessage={errorMessage} />
    </RAC.NumberField>
  );
}

export type Option = { id: string; label: ReactNode; textValue?: string; description?: ReactNode };

export type SelectProps = Omit<RAC.SelectProps<Option>, "className" | "children"> &
  FieldChrome & { options: Option[] };

export function Select({
  label,
  description,
  errorMessage,
  options,
  className,
  ...props
}: SelectProps) {
  return (
    <RAC.Select
      {...props}
      isInvalid={props.isInvalid || Boolean(errorMessage)}
      className={cx("ui-field", className)}
    >
      <RAC.Label className="ui-label">{label}</RAC.Label>
      <RAC.Button className="ui-select-trigger">
        <RAC.SelectValue className="ui-select-value" />
        <Icon name="chevronDown" size={18} />
      </RAC.Button>
      <FieldFooter description={description} errorMessage={errorMessage} />
      <RAC.Popover className="ui-popover">
        <RAC.ListBox className="ui-listbox" items={options}>
          {(o) => (
            <RAC.ListBoxItem
              id={o.id}
              className="ui-option"
              textValue={o.textValue ?? (typeof o.label === "string" ? o.label : o.id)}
            >
              <span className="ui-option-label">{o.label}</span>
              <Icon name="check" size={18} className="ui-option-check" />
            </RAC.ListBoxItem>
          )}
        </RAC.ListBox>
      </RAC.Popover>
    </RAC.Select>
  );
}

export type RadioGroupProps = Omit<RAC.RadioGroupProps, "className" | "children"> &
  FieldChrome & { options: Option[] };

export function RadioGroup({
  label,
  description,
  errorMessage,
  options,
  className,
  ...props
}: RadioGroupProps) {
  return (
    <RAC.RadioGroup
      {...props}
      isInvalid={props.isInvalid || Boolean(errorMessage)}
      className={cx("ui-field ui-radio-group", className)}
    >
      <RAC.Label className="ui-label">{label}</RAC.Label>
      <div className="ui-radio-options">
        {options.map((o) => (
          <RAC.Radio key={o.id} value={o.id} className="ui-radio">
            <span className="ui-radio-mark" aria-hidden="true" />
            <span className="ui-choice-text">
              <span className="ui-choice-label">{o.label}</span>
              {o.description && <span className="ui-choice-description">{o.description}</span>}
            </span>
          </RAC.Radio>
        ))}
      </div>
      <FieldFooter description={description} errorMessage={errorMessage} />
    </RAC.RadioGroup>
  );
}

export type CheckboxProps = Omit<RAC.CheckboxProps, "className" | "children"> & {
  children: ReactNode;
  description?: ReactNode;
  className?: string;
};

export function Checkbox({ children, description, className, ...props }: CheckboxProps) {
  return (
    <RAC.Checkbox {...props} className={cx("ui-checkbox", className)}>
      <span className="ui-checkbox-box" aria-hidden="true">
        <Icon name="check" size={16} />
      </span>
      <span className="ui-choice-text">
        <span className="ui-choice-label">{children}</span>
        {description && <span className="ui-choice-description">{description}</span>}
      </span>
    </RAC.Checkbox>
  );
}

export type SwitchProps = Omit<RAC.SwitchProps, "className" | "children"> & {
  children: ReactNode;
  className?: string;
};

export function Switch({ children, className, ...props }: SwitchProps) {
  return (
    <RAC.Switch {...props} className={cx("ui-switch", className)}>
      <span className="ui-switch-track" aria-hidden="true">
        <span className="ui-switch-thumb" />
      </span>
      <span className="ui-choice-label">{children}</span>
    </RAC.Switch>
  );
}

/* Overlays */

export type DialogProps = {
  title: ReactNode;
  children: ReactNode | ((close: () => void) => ReactNode);
  isOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  /** Dismiss on outside click and Escape. Off for dialogs that guard data. */
  isDismissable?: boolean;
  role?: "dialog" | "alertdialog";
  size?: "md" | "lg";
};

/** A modal dialog. Use inside RAC's DialogTrigger, or control it with isOpen. */
export function Dialog({
  title,
  children,
  isOpen,
  onOpenChange,
  isDismissable = true,
  role,
  size = "md",
}: DialogProps) {
  return (
    <RAC.ModalOverlay
      className="ui-overlay"
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      isDismissable={isDismissable && role !== "alertdialog"}
    >
      <RAC.Modal className="ui-modal" data-size={size}>
        <RAC.Dialog className="ui-dialog" role={role}>
          {({ close }) => (
            <>
              <div className="ui-dialog-head">
                <RAC.Heading slot="title" className="ui-dialog-title">
                  {title}
                </RAC.Heading>
                {role !== "alertdialog" && (
                  <RAC.Button
                    className="ui-icon-button"
                    aria-label={strings.buttons.close}
                    onPress={close}
                  >
                    <Icon name="close" />
                  </RAC.Button>
                )}
              </div>
              <div className="ui-dialog-body">
                {typeof children === "function" ? children(close) : children}
              </div>
            </>
          )}
        </RAC.Dialog>
      </RAC.Modal>
    </RAC.ModalOverlay>
  );
}

export const DialogTrigger = RAC.DialogTrigger;

/* Tabs */

export function Tabs({
  className,
  ...props
}: Omit<RAC.TabsProps, "className"> & { className?: string }) {
  return <RAC.Tabs {...props} className={cx("ui-tabs", className)} />;
}

export function TabList<T extends object>({
  className,
  ...props
}: Omit<RAC.TabListProps<T>, "className"> & { className?: string }) {
  return <RAC.TabList {...props} className={cx("ui-tablist", className)} />;
}

export function Tab({
  className,
  ...props
}: Omit<RAC.TabProps, "className"> & { className?: string }) {
  return <RAC.Tab {...props} className={cx("ui-tab", className)} />;
}

export function TabPanel({
  className,
  ...props
}: Omit<RAC.TabPanelProps, "className"> & { className?: string }) {
  return <RAC.TabPanel {...props} className={cx("ui-tabpanel", className)} />;
}

/* Progress */

export type ProgressBarProps = Omit<RAC.ProgressBarProps, "className" | "children"> & {
  label: ReactNode;
  /** Show the percentage next to the label. */
  showValue?: boolean;
  className?: string;
};

/** Leave `value` undefined for an indeterminate bar. */
export function ProgressBar({ label, showValue = true, className, ...props }: ProgressBarProps) {
  return (
    <RAC.ProgressBar
      {...props}
      isIndeterminate={props.isIndeterminate ?? props.value === undefined}
      className={cx("ui-progress", className)}
    >
      {({ percentage, valueText, isIndeterminate }) => (
        <>
          <div className="ui-progress-head">
            <RAC.Label className="ui-label">{label}</RAC.Label>
            {showValue && !isIndeterminate && (
              <span className="ui-progress-value tabular">{valueText}</span>
            )}
          </div>
          <div className="ui-progress-track">
            <div
              className="ui-progress-fill"
              data-indeterminate={isIndeterminate || undefined}
              style={isIndeterminate ? undefined : { inlineSize: `${percentage ?? 0}%` }}
            />
          </div>
        </>
      )}
    </RAC.ProgressBar>
  );
}

export type MeterProps = Omit<RAC.MeterProps, "className" | "children"> & {
  label: ReactNode;
  /** Any CSS colour (for example a marker's own colour). Defaults to the accent. */
  color?: string;
  className?: string;
};

export function Meter({ label, color, className, ...props }: MeterProps) {
  return (
    <RAC.Meter {...props} className={cx("ui-meter", className)}>
      {({ percentage, valueText }) => (
        <>
          <div className="ui-progress-head">
            <RAC.Label className="ui-label">{label}</RAC.Label>
            <span className="ui-progress-value tabular">{valueText}</span>
          </div>
          <div className="ui-progress-track">
            <div
              className="ui-progress-fill"
              style={{ inlineSize: `${percentage}%`, ...(color ? { background: color } : {}) }}
            />
          </div>
        </>
      )}
    </RAC.Meter>
  );
}

/* Toasts */

export type ToastContent = {
  title: string;
  description?: string;
  tone?: "neutral" | "ok" | "problem";
};

export const toastQueue = new RAC.UNSTABLE_ToastQueue<ToastContent>({ maxVisibleToasts: 3 });

/** Shows a toast. Problems stay until dismissed; everything else leaves after 5 s. */
export function toast(content: ToastContent) {
  return toastQueue.add(content, content.tone === "problem" ? undefined : { timeout: 5000 });
}

export function ToastRegion() {
  return (
    <RAC.UNSTABLE_ToastRegion queue={toastQueue} className="ui-toast-region">
      {({ toast: t }) => (
        <RAC.UNSTABLE_Toast toast={t} className="ui-toast" data-tone={t.content.tone ?? "neutral"}>
          <RAC.UNSTABLE_ToastContent className="ui-toast-content">
            <RAC.Text slot="title" className="ui-toast-title">
              {t.content.title}
            </RAC.Text>
            {t.content.description && (
              <RAC.Text slot="description" className="ui-toast-description">
                {t.content.description}
              </RAC.Text>
            )}
          </RAC.UNSTABLE_ToastContent>
          <RAC.Button slot="close" className="ui-icon-button" aria-label={strings.buttons.dismiss}>
            <Icon name="close" size={18} />
          </RAC.Button>
        </RAC.UNSTABLE_Toast>
      )}
    </RAC.UNSTABLE_ToastRegion>
  );
}

/* Files */

export function DropZone({
  className,
  ...props
}: Omit<RAC.DropZoneProps, "className"> & { className?: string }) {
  return <RAC.DropZone {...props} className={cx("ui-dropzone", className)} />;
}

export const FileTrigger = RAC.FileTrigger;

/* Disclosure */

export type DisclosureProps = Omit<RAC.DisclosureProps, "className" | "children"> & {
  title: ReactNode;
  children: ReactNode;
  className?: string;
};

export function Disclosure({ title, children, className, ...props }: DisclosureProps) {
  return (
    <RAC.Disclosure {...props} className={cx("ui-disclosure", className)}>
      <RAC.Heading level={3} className="ui-disclosure-heading">
        <RAC.Button slot="trigger" className="ui-disclosure-trigger">
          <span>{title}</span>
          <Icon name="chevronDown" size={18} className="ui-disclosure-chevron" />
        </RAC.Button>
      </RAC.Heading>
      <RAC.DisclosurePanel className="ui-disclosure-panel">{children}</RAC.DisclosurePanel>
    </RAC.Disclosure>
  );
}

/* Layout and status pieces */

export type CardProps = {
  title?: ReactNode;
  /** Controls placed at the end of the card's title row. */
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  tone?: "default" | "problem";
  /** Heading level of the title (default 2). */
  level?: 2 | 3;
};

/** A sheet of content. Never nest cards: group inside one with headings or rules. */
export function Card({
  title,
  actions,
  children,
  className,
  tone = "default",
  level = 2,
}: CardProps) {
  const H = level === 2 ? "h2" : "h3";
  return (
    <section className={cx("ui-card", className)} data-tone={tone}>
      {(title || actions) && (
        <header className="ui-card-head">
          {title && <H className="ui-card-title">{title}</H>}
          {actions && <div className="ui-card-actions">{actions}</div>}
        </header>
      )}
      <div className="ui-card-body">{children}</div>
    </section>
  );
}

export type Tone = "ok" | "busy" | "problem" | "idle";

/** A coloured dot plus a text label; the colour never carries meaning alone. */
export function StatusBadge({
  tone,
  children,
  pulse = false,
}: {
  tone: Tone;
  children: ReactNode;
  pulse?: boolean;
}) {
  return (
    <span className="ui-badge" data-tone={tone}>
      <span className="ui-badge-dot" data-pulse={pulse || undefined} aria-hidden="true" />
      <span className="ui-badge-label">{children}</span>
    </span>
  );
}

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header className="ui-page-header">
      <div className="ui-page-heading">
        <h1 tabIndex={-1}>{title}</h1>
        {description && <p className="ui-page-description">{description}</p>}
      </div>
      {actions && <div className="ui-page-actions">{actions}</div>}
    </header>
  );
}

export function EmptyState({
  icon,
  title,
  children,
  action,
}: {
  icon?: IconName;
  title: ReactNode;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="ui-empty">
      {icon && <Icon name={icon} size={28} className="ui-empty-icon" />}
      <p className="ui-empty-title">{title}</p>
      {children && <div className="ui-empty-body">{children}</div>}
      {action && <div className="ui-empty-action">{action}</div>}
    </div>
  );
}

export function InlineError({ children, action }: { children: ReactNode; action?: ReactNode }) {
  return (
    <div className="ui-inline-error" role="alert">
      <Icon name="alert" size={18} className="ui-inline-error-icon" />
      <div className="ui-inline-error-text">{children}</div>
      {action && <div className="ui-inline-error-action">{action}</div>}
    </div>
  );
}
