"""Select user categories for feature access."""

from authentik.core.models import User, UserTypes


def matches_user_type(
    user: User,
    *,
    service_accounts: bool = False,
    internal_service_accounts: bool = False,
    agents: bool = False,
) -> bool:
    """Match selected categories, treating agents separately from service accounts.

    Agents have the service-account type, but are selected only by `agents`.
    Ordinary service accounts, including other actors, use `service_accounts`.
    """
    if user.type == UserTypes.INTERNAL_SERVICE_ACCOUNT:
        return internal_service_accounts
    if user.type != UserTypes.SERVICE_ACCOUNT:
        return False
    if service_accounts == agents:
        return service_accounts
    is_agent = hasattr(user, "actor") and hasattr(user.actor, "agent")
    return agents if is_agent else service_accounts
