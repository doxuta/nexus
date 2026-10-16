// Registry of /dev/kit showcase sections — one entry per SLDS component family in
// components/slds/. Each section renders the Nexus wrapper on STATIC sample data and names the
// blueprint/variant it copies (specs/038 FR-006). Keep entries sorted by id; one line each.
import type { ComponentType } from "react";
import { AssistantSection } from "./AssistantSection";
import { BuildersSection } from "./BuildersSection";
import { ButtonsSection } from "./ButtonsSection";
import { DailyChartSection } from "./DailyChartSection";
import { DataTableSection } from "./DataTableSection";
import { FeedbackContentSection } from "./FeedbackContentSection";
import { FormInputsSection } from "./FormInputsSection";
import { IconSection } from "./IconSection";
import { OverlaysSection } from "./OverlaysSection";
import { PageStructureSection } from "./PageStructureSection";
import { PickersSection } from "./PickersSection";
import { RelationMapSection } from "./RelationMapSection";
import { ScreenEditorSection } from "./ScreenEditorSection";
import { ShellSection } from "./ShellSection";
import { StudioNavSection } from "./StudioNavSection";
import { StudioSection } from "./StudioSection";
import { StudioTrialHandoffSection } from "./StudioTrialHandoffSection";
import { TemplatesSection } from "./TemplatesSection";

export interface KitSection {
  id: string;
  title: string;
  /** SLDS blueprint(s) the wrappers in this section copy, e.g. "buttons, button-icons". */
  blueprint: string;
  Component: ComponentType;
}

export const SECTIONS: KitSection[] = [
  { id: "assistant", title: "Thẻ đề xuất của trợ lý", blueprint: "cards/base, scoped-notifications (xác nhận tại chỗ), spinners", Component: AssistantSection },
  { id: "builders", title: "Trình dựng", blueprint: "panels/filtering, expression, dueling-picklist, file-selector, drop-zone, visual-picker", Component: BuildersSection },
  { id: "buttons", title: "Nút & nhãn", blueprint: "buttons, button-icons, button-groups, badges, avatar, pills, spinners, progress-bar", Component: ButtonsSection },
  { id: "daily-chart", title: "Biểu đồ theo ngày", blueprint: "không có blueprint SLDS: đường 2px, lưới nhạt, nhãn điểm cuối, popovers/tooltips, data-tables (bảng thay thế)", Component: DailyChartSection },
  { id: "data-table", title: "Bảng dữ liệu", blueprint: "data-tables (advanced, inline-edit), tree-grid", Component: DataTableSection },
  { id: "feedback-content", title: "Phản hồi & nội dung", blueprint: "expandable-section, accordion, alert, scoped-notifications, illustration, docked-form-footer, progress-indicator, activity-timeline", Component: FeedbackContentSection },
  { id: "form-inputs", title: "Form & ô nhập", blueprint: "form-element, input, textarea, checkbox, checkbox-toggle, radio-group, radio-button-group, select", Component: FormInputsSection },
  { id: "icons", title: "Icon", blueprint: "icons/standard, icons/utility", Component: IconSection },
  { id: "overlays", title: "Overlays", blueprint: "modals, prompt, panels, menus, popovers, tooltips, toast", Component: OverlaysSection },
  { id: "page-structure", title: "Cấu trúc trang", blueprint: "breadcrumbs, cards, page-headers, tabs, scoped-tabs, vertical-tabs, path", Component: PageStructureSection },
  { id: "pickers", title: "Chọn giá trị", blueprint: "combobox, lookups, picklist, datepickers, timepicker, datetime-picker", Component: PickersSection },
  { id: "relation-map", title: "Sơ đồ liên kết", blueprint: "drop-zone, cards/base, buttons, scoped-notifications (lớp token Studio)", Component: RelationMapSection },
  { id: "screen-editor", title: "Trình sửa màn hình", blueprint: "drop-zone (container hover/selected), buttons, select, menus, form-element", Component: ScreenEditorSection },
  { id: "shell", title: "Khung ứng dụng", blueprint: "global-header, global-navigation/navigation-bar, app-launcher", Component: ShellSection },
  { id: "studio", title: "Studio", blueprint: "lớp token Studio: buttons, cards, tabs, input, modals/prompt (chỉ gán giá trị hook)", Component: StudioSection },
  { id: "studio-nav", title: "Điều hướng Studio", blueprint: "path/base, tabs/base, buttons/stateful, global-header/base", Component: StudioNavSection },
  { id: "studio-trial", title: "Chạy thử: dữ liệu mẫu", blueprint: "data-tables/base (chỉ đọc, xếp chồng dưới 768px), menus", Component: StudioTrialHandoffSection },
  { id: "templates", title: "Template trang", blueprint: "page-headers, path, tabs, expandable-section, scoped-notifications, docked-form-footer, modals", Component: TemplatesSection },
];
