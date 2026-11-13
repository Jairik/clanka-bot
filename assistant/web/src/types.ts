// Mirrors the Go view models in internal/dashboard (encoding/json default
// field names). The server owns every label, source rule and action URL; the
// app only lays them out.

export type Session = { Owner: string; Typed: boolean; Mic: boolean; Volume: boolean; Activity: boolean };

export type Volume = { Available: boolean; Percent: number; Muted: boolean; Error: string };

/** Every page view carries the result of the last write. */
export type Flashable = { Flash?: string; Error?: string };

// ---------- activity ----------
export type Timing = { Name: string; Label: string; MS: string };
export type Call = { Name: string; LocalOnly: boolean; Result: string };
export type Request = {
  ID: string;
  Time: string;
  TimeISO: string;
  Source: string;
  SourceLabel: string;
  Path: string;
  PathLabel: string;
  Tool: string;
  Outcome: string;
  OutcomeText: string;
  Response: string;
  Measured: boolean;
  Error: string;
  Transcript: string;
  Reply: string;
  LocalOnly: boolean;
  Pending: boolean;
  Stages: Timing[] | null;
  Checkpoints: Timing[] | null;
  Calls: Call[] | null;
};
export type ActivityView = Flashable & {
  Notice: string;
  Draft: string;
  HasActivity: boolean;
  HasTyped: boolean;
  DisplayLimit: number;
  Just: Request | null;
  Requests: Request[] | null;
  JustRequestID: string;
  PrivacyNotice: string;
  LatencyNotice: string;
  CollectionNote: string;
};

// ---------- health ----------
export type StatusState = "ok" | "error" | "not-configured" | "timed-out" | "no-check";
export type Check = { Name: string; Status: StatusState; StatusLabel: string; Detail: string };
export type HealthView = {
  CheckedAt: string;
  HealthTimeout: string;
  Services: Check[] | null;
  Location: Check;
  Modules: Check[] | null;
  Spotify: Check[] | null;
};

// ---------- settings ----------
export type Choice = { Value: string; Label: string; Selected: boolean };
export type SettingTool = {
  Name: string;
  Enabled: boolean;
  EnabledSource: string;
  EnabledLabel: string;
  EnabledLower: string;
  EnabledOverride: boolean;
  Permission: string;
  PermissionLabel: string;
  Declared: string;
  DeclaredLabel: string;
  PermissionSource: string;
  PermissionLabelSource: string;
  PermissionLower: string;
  PermissionOverride: boolean;
  LocalOnly: boolean;
  LocalOnlyLabel: string;
  Options: Choice[] | null;
};
export type Setting = {
  Key: string;
  Anchor: string;
  Label: string;
  Description: string;
  FormKind: "lines" | "bool" | "number" | "decimal" | "duration" | "text" | string;
  Value: string;
  Edit: string;
  Display: string;
  Bool: boolean;
  Source: string;
  SourceLabel: string;
  LowerLabel: string;
  Restart: boolean;
  Override: boolean;
  SaveAction: string;
  ResetAction: string;
};
export type ToolWording = { Name: string; Anchor: string; Control: Setting | null; Examples: Setting | null };
export type SettingsView = Flashable & {
  HasSettings: boolean;
  Tools: SettingTool[] | null;
  Agent: Setting[] | null;
  ToolWording: ToolWording[] | null;
  Quiet: Setting[] | null;
  Live: Setting[] | null;
  Restart: Setting[] | null;
};

// ---------- secrets ----------
export type Secret = {
  Name: string;
  Anchor: string;
  Set: boolean;
  SetLabel: string;
  Source: string;
  SourceLabel: string;
  Last4: string;
  Updated: string;
  UpdatedISO: string;
  Stored: boolean;
  SetAction: string;
  ClearAction: string;
};
export type SecretsView = Flashable & { HasSecrets: boolean; Secrets: Secret[] | null };

// ---------- sounds ----------
export type SoundRow = { Name: string; Duration: string; Added: string; Origin: string; AudioURL: string };
export type SoundRole = {
  Role: string;
  Label: string;
  Key: string;
  Cap: string;
  Display: string;
  Source: string;
  SourceLabel: string;
  LowerLabel: string;
  Override: boolean;
  Options: Choice[] | null;
};
export type RingKind = { Kind: string; Label: string; Settings: Setting[] | null };
export type SoundsView = Flashable & {
  HasLibrary: boolean;
  MaxUpload: string;
  Sounds: SoundRow[] | null;
  Roles: SoundRole[] | null;
  HasRoles: boolean;
  RingKinds: RingKind[] | null;
  HasRing: boolean;
};

// ---------- voices ----------
export type VoiceRow = {
  Name: string;
  Anchor: string;
  Origin: string;
  Status: string;
  Revision: string;
  Validated: string;
  Added: string;
  Note: string;
  LastError: string;
  StaleNotice: string;
  Builtin: boolean;
  Ready: boolean;
  Stale: boolean;
  Active: boolean;
  Unvalidated: boolean;
  PreviewURL: string;
  SelectAction: string;
  NoteAction: string;
};
export type VoicesView = Flashable & {
  HasLibrary: boolean;
  CanUpload: boolean;
  CanPreview: boolean;
  MaxUpload: string;
  Revision: string;
  ActiveName: string;
  Default: string;
  Rate: string;
  RateSource: string;
  RateLabel: string;
  RateLower: string;
  RateEdit: string;
  HasRate: boolean;
  Voices: VoiceRow[] | null;
};

// ---------- personas ----------
export type PersonaForm = {
  Action: string;
  Heading: string;
  FormID: string;
  Name: string;
  NameLocked: boolean;
  Voice: string;
  Personality: string;
  Filler: string;
  Unavailable: string;
  Submit: string;
  Voices: { Name: string; Label: string; Selected: boolean }[] | null;
};
export type PersonasView = Flashable & {
  HasService: boolean;
  Active: { Name: string; Voice: string; EffectiveVoice: string; Exact: boolean; Notes: string[] | null } | null;
  New: PersonaForm;
  Rows: { Name: string; Voice: string; VoiceStatus: string; Active: boolean; SelectAction: string; Edit: PersonaForm }[] | null;
};

// ---------- alerts ----------
export type ScheduledAlert = {
  KindKey: string;
  Kind: string;
  Label: string;
  Next: string;
  NextISO: string;
  Detail: string;
  CancelAction: string;
  CancelLabel: string;
};
export type AlarmEditor = {
  ID: string;
  Title: string;
  Schedule: string;
  Next: string;
  NextISO: string;
  SoundCurrent: string;
  SoundEffective: string;
  SoundDefault: string;
  SoundOverride: boolean;
  VolumeCurrent: string;
  VolumeEffective: string;
  VolumeDefault: string;
  VolumeOverride: boolean;
  RampCurrent: string;
  RampEffective: string;
  RampDefault: string;
  RampOverride: boolean;
  HasOverride: boolean;
  SoundKey: string;
  VolumeKey: string;
  RampKey: string;
  SoundCap: string;
  HasLibrary: boolean;
  Sounds: Choice[] | null;
  Action: string;
  ResetAction: string;
  Edit: { AlarmID: string; Sound: string; Volume: string; Ramp: string; VolumeReset: boolean; RampReset: boolean };
};
export type MissedAlert = {
  Kind: string;
  Label: string;
  Scheduled: string;
  ScheduledISO: string;
  Fired: string;
  FiredISO: string;
  Ended: string;
  EndedISO: string;
  EndedBy: string;
};
export type AlertsView = Flashable & {
  HasManagers: boolean;
  ZoneName: string;
  Alerts: ScheduledAlert[] | null;
  Alarms: AlarmEditor[] | null;
  Missed: MissedAlert[] | null;
  MissedLimit: number;
};

// ---------- spotify ----------
export type Pill = "ok" | "error" | "not-configured";
export type SpotifyView = Flashable & {
  Pairing: {
    Configured: boolean;
    Available: boolean;
    Paired: boolean;
    Pill: Pill;
    Label: string;
    Detail: string;
    Device: string;
    Updated: string;
    UpdatedISO: string;
  };
  WebAPI: {
    State: string;
    Pill: Pill;
    Label: string;
    Detail: string;
    ClientConfigured: boolean;
    RedirectConfigured: boolean;
    RedirectURI: string;
    Set: boolean;
    Source: string;
    SourceLabel: string;
    Updated: string;
    UpdatedISO: string;
    LastRefresh: string;
    LastRefreshISO: string;
    Expires: string;
    ExpiresISO: string;
    CanConnect: boolean;
    CanVerify: boolean;
    CanDisconnect: boolean;
  };
  Scopes: string[] | null;
};

export type PageViews = {
  activity: ActivityView;
  health: HealthView;
  settings: SettingsView;
  secrets: SecretsView;
  sounds: SoundsView;
  voices: VoicesView;
  personas: PersonasView;
  alerts: AlertsView;
  spotify: SpotifyView;
};
export type DataPage = keyof PageViews;
