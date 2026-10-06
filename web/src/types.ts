// Shapes from docs/api.md. Keep these in step with the contract.

export interface ReceiverSummary {
  key: string;
  provider: string;
  callsign: string;
  name: string;
  location: string;
  country: string;
  lat: number;
  lon: number;
  publicUrl: string;
  distanceKm?: number | null;
  score?: number;
  reasons?: string[];
  availableClients: number;
  maxClients: number;
  deepLink?: string;
}

export type SignalState = "unknown" | "present" | "absent";

export interface Signal {
  state: SignalState;
  snr?: number;
  at?: string;
  receiverKey?: string;
}

export interface EventFreq {
  hz: number;
  signal: Signal;
  receivers: ReceiverSummary[];
}

export type EventStatus = "upcoming" | "live" | "done";

export interface ScheduleEvent {
  id: string;
  station: string;
  stationName: string;
  priyomUrl: string | null;
  language?: string;
  category?: string;
  start: string;
  end: string;
  endEstimated: boolean;
  status: EventStatus;
  search: boolean;
  freqs: EventFreq[];
  priyomMode: string;
  mode: string;
  digital: boolean;
  remarks: string[];
  target: string | null;
  raw: string;
  parsed: boolean;
}

export interface NowResponse {
  serverTime: string;
  scheduleUpdated: string;
  now: ScheduleEvent[];
  next: ScheduleEvent[];
  later: ScheduleEvent[];
}

export interface Capabilities {
  historicalSpectrogram: boolean;
  liveSpectrum: boolean;
}

export interface ChannelResponse {
  channelId: string;
  listenerId: string;
  receiver: ReceiverSummary;
  freqHz: number;
  mode: string;
  spanHz: number;
  capabilities: Capabilities;
  alternatives: ReceiverSummary[];
}

export interface SpectrumHeader {
  type: "header";
  startHz: number;
  binHz: number;
  bins: number;
  centerHz: number;
  tunedHz: number;
  dbMin: number;
  dbMax: number;
}

export interface SpectrumError {
  type: "error";
  error: string;
  reason?: string;
}
