// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import type { Hypeman } from '../client';

export abstract class APIResource {
  protected _client: Hypeman;

  constructor(client: Hypeman) {
    this._client = client;
  }
}
