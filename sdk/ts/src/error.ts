/** SayError carries the API's error code, e.g. "invalid_credentials". */
export class SayError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
    this.name = "SayError";
  }
}
