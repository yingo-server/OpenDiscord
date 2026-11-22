/**
 * 登录错误 → i18n 文案。后端 code 稳定，文案由前端 catalog 提供。
 */
import { AuthError } from '../../../net/auth/provider';
import { t } from '../../i18n';

const KNOWN = new Set([
  'INVALID_PHONE',
  'CODE_INVALID',
  'BAD_CREDENTIALS',
  'CAPTCHA_REQUIRED',
  'PENDING',
  'NETWORK',
]);

export function authErrorText(error: unknown): string {
  if (error instanceof AuthError) {
    if (KNOWN.has(error.code)) return t(`auth.error.${error.code}`);
    // 未知后端 code 一律显示通用文案：后端 message 可能含 driver / SQL /
    // 协议等技术细节，不直接暴露给终端用户（"有的提示不该让用户看到"）。
    return t('auth.error.DEFAULT');
  }
  if (error instanceof TypeError) {
    return t('auth.error.NETWORK');
  }
  return t('auth.error.DEFAULT');
}
